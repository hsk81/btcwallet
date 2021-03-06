/*
 * Copyright (c) 2013, 2014 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwallet/tx"
	"github.com/conformal/btcwallet/wallet"
	"github.com/conformal/btcwire"
	"os"
	"sync"
)

// Errors relating to accounts.
var (
	ErrAcctExists   = errors.New("account already exists")
	ErrAcctNotExist = errors.New("account does not exist")
)

var accountstore = NewAccountStore()

// AccountStore stores all wallets currently being handled by
// btcwallet.  Wallet are stored in a map with the account name as the
// key.  A RWMutex is used to protect against incorrect concurrent
// access.
type AccountStore struct {
	sync.RWMutex
	accounts map[string]*Account
}

// NewAccountStore returns an initialized and empty AccountStore.
func NewAccountStore() *AccountStore {
	return &AccountStore{
		accounts: make(map[string]*Account),
	}
}

// Account returns the account specified by name, or ErrAcctNotExist
// as an error if the account is not found.
func (store *AccountStore) Account(name string) (*Account, error) {
	store.RLock()
	defer store.RUnlock()

	account, ok := store.accounts[name]
	if !ok {
		return nil, ErrAcctNotExist
	}
	return account, nil
}

// Rollback rolls back each Account saved in the store.
func (store *AccountStore) Rollback(height int32, hash *btcwire.ShaHash) {
	log.Debugf("Rolling back tx history since block height %v hash %v",
		height, hash)

	store.RLock()
	defer store.RUnlock()

	for _, account := range store.accounts {
		account.Rollback(height, hash)
	}
}

// BlockNotify runs after btcwallet is notified of a new block connected to
// the best chain.  It notifies all frontends of any changes from the new
// block, including changed balances.  Each account is then set to be synced
// with the latest block.
func (store *AccountStore) BlockNotify(bs *wallet.BlockStamp) {
	store.RLock()
	defer store.RUnlock()

	for name, a := range store.accounts {
		// TODO: need a flag or check that the utxo store was actually
		// modified, or this will notify even if there are no balance
		// changes, or sending these notifications as the utxos are added.
		confirmed := a.CalculateBalance(1)
		unconfirmed := a.CalculateBalance(0) - confirmed
		NotifyWalletBalance(frontendNotificationMaster, a.name, confirmed)
		NotifyWalletBalanceUnconfirmed(frontendNotificationMaster, a.name,
			unconfirmed)

		// If this is the default account, update the block all accounts
		// are synced with, and schedule a wallet write.
		if name == "" {
			a.mtx.Lock()
			a.Wallet.SetSyncedWith(bs)
			a.mtx.Unlock()
			a.ScheduleWalletWrite()
		}
	}
}

// RecordMinedTx searches through each account's TxStore, searching for a
// sent transaction with the same txid as from a txmined notification.  If
// the transaction IDs match, the record in the TxStore is updated with
// the full information about the newly-mined tx, and the TxStore is
// scheduled to be written to disk..
func (store *AccountStore) RecordMinedTx(txid *btcwire.ShaHash,
	blkhash *btcwire.ShaHash, blkheight int32, blkindex int,
	blktime int64) error {

	store.RLock()
	defer store.RUnlock()

	for _, account := range store.accounts {
		// The tx stores will be searched through while holding the
		// reader lock, and the writer will only be grabbed if necessary.
		account.TxStore.RLock()

		// Search in reverse order.  Since more recently-created
		// transactions are appended to the end of the store, it's
		// more likely to find it when searching from the end.
		for i := len(account.TxStore.s) - 1; i >= 0; i-- {
			sendtx, ok := account.TxStore.s[i].(*tx.SendTx)
			if ok {
				if bytes.Equal(txid.Bytes(), sendtx.TxID[:]) {
					account.TxStore.RUnlock()

					account.TxStore.Lock()
					copy(sendtx.BlockHash[:], blkhash.Bytes())
					sendtx.BlockHeight = blkheight
					sendtx.BlockIndex = int32(blkindex)
					sendtx.BlockTime = blktime
					account.TxStore.Unlock()

					account.ScheduleTxStoreWrite()

					return nil
				}
			}
		}

		account.TxStore.RUnlock()
	}

	return errors.New("txid does not match any recorded sent transaction")
}

// CalculateBalance returns the balance, calculated using minconf
// block confirmations, of an account.
func (store *AccountStore) CalculateBalance(account string,
	minconf int) (float64, error) {

	a, err := store.Account(account)
	if err != nil {
		return 0, err
	}

	return a.CalculateBalance(minconf), nil
}

// CreateEncryptedWallet creates a new account with a wallet file
// encrypted with passphrase.
func (store *AccountStore) CreateEncryptedWallet(name, desc string, passphrase []byte) error {
	store.RLock()
	_, ok := store.accounts[name]
	store.RUnlock()
	if ok {
		return ErrAcctExists
	}

	// Get current block's height and hash.
	bs, err := GetCurBlock()
	if err != nil {
		return err
	}

	// Create new wallet in memory.
	wlt, err := wallet.NewWallet(name, desc, passphrase, cfg.Net(), &bs, cfg.KeypoolSize)
	if err != nil {
		return err
	}

	// Create new account with the wallet.  A new JSON ID is set for
	// transaction notifications.
	account := &Account{
		Wallet: wlt,
		name:   name,
	}
	account.ScheduleWalletWrite()
	account.ScheduleTxStoreWrite()
	account.ScheduleUtxoStoreWrite()

	// Mark all active payment addresses as belonging to this account.
	for addr := range account.ActivePaymentAddresses() {
		MarkAddressForAccount(addr, name)
	}

	// Save the account in the global account map.  The mutex is
	// already held at this point, and will be unlocked when this
	// func returns.
	store.Lock()
	store.accounts[name] = account
	store.Unlock()

	// Begin tracking account against a connected btcd.
	//
	// TODO(jrick): this should *only* happen if btcd is connected.
	account.Track()

	// Ensure that the account is written out to disk.
	if err := account.WriteScheduledToDisk(); err != nil {
		return err
	}

	return nil
}

// ChangePassphrase unlocks all account wallets with the old
// passphrase, and re-encrypts each using the new passphrase.
//
// TODO(jrick): this is a perfect example of how awful the account
// locking is.  It must be replaced.
func (store *AccountStore) ChangePassphrase(old, new []byte) error {
	// Due to the undefined order of ranging over the accountstore
	// map and how all account wallet writer locks are grabbed
	// simultaneously and unlocked with a defer, this function is
	// unsafe to call simulateously with other accountstore functions,
	// even though the store itself is not modified.
	store.Lock()
	defer store.Unlock()

	if err := store.changePassphrase(old, new); err != nil {
		return err
	}

	// Immediately write out to disk.
	return store.WriteAllToDisk()
}

// changePassphrase changes all passphrases for all accounts without grabbing
// any accountstore locks.
func (store *AccountStore) changePassphrase(old, new []byte) error {
	// Check that each account can be unlocked with the old passphrase.
	for _, a := range store.accounts {
		a.mtx.Lock()
		defer a.mtx.Unlock()

		if locked := a.Wallet.IsLocked(); !locked {
			if err := a.Wallet.Lock(); err != nil {
				return err
			}
		}

		if err := a.Wallet.Unlock(old); err != nil {
			return err
		}
		defer a.Wallet.Lock()
	}

	// Change passphrase for each unlocked wallet.
	for _, a := range store.accounts {
		if err := a.Wallet.ChangePassphrase(new); err != nil {
			return err
		}
	}

	return nil
}

// LockWallets locks all account's wallets in the store.
func (store *AccountStore) LockWallets() error {
	store.RLock()
	defer store.RUnlock()

	for _, a := range store.accounts {
		if err := a.Lock(); err != nil {
			return err
		}
	}

	return nil
}

// UnlockWallets unlocks all account's wallets in the store with the provided
// passphrase.  If any wallet unlocks fail, all successfully unlocked wallets
// are locked again.
func (store *AccountStore) UnlockWallets(passphrase string) error {
	store.RLock()
	defer store.RUnlock()

	unlockedAccts := make([]*Account, 0, len(store.accounts))
	for _, a := range store.accounts {
		if err := a.Unlock([]byte(passphrase)); err != nil {
			for _, ua := range unlockedAccts {
				ua.Lock()
			}
			return fmt.Errorf("cannot unlock account %v: %v",
				a.name, err)
		}
		unlockedAccts = append(unlockedAccts, a)
	}

	return nil
}

// DumpKeys returns all WIF-encoded private keys associated with all
// accounts. All wallets must be unlocked for this operation to succeed.
func (store *AccountStore) DumpKeys() ([]string, error) {
	store.RLock()
	defer store.RUnlock()

	var keys []string
	for _, a := range store.accounts {
		switch walletKeys, err := a.DumpPrivKeys(); err {
		case wallet.ErrWalletLocked:
			return nil, err

		case nil:
			keys = append(keys, walletKeys...)

		default: // any other non-nil error
			return nil, err
		}

	}
	return keys, nil
}

// DumpWIFPrivateKey searches through all accounts for the bitcoin
// payment address addr and returns the WIF-encdoded private key.
func (store *AccountStore) DumpWIFPrivateKey(addr btcutil.Address) (string, error) {
	store.RLock()
	defer store.RUnlock()

	for _, a := range store.accounts {
		switch wif, err := a.DumpWIFPrivateKey(addr); err {
		case wallet.ErrAddressNotFound:
			// Move on to the next account.
			continue

		case nil:
			return wif, nil

		default: // all other non-nil errors
			return "", err
		}
	}

	return "", errors.New("address does not refer to a key")
}

// NotifyBalances notifies a wallet frontend of all confirmed and unconfirmed
// account balances.
func (store *AccountStore) NotifyBalances(frontend chan []byte) {
	store.RLock()
	defer store.RUnlock()

	for _, account := range store.accounts {
		balance := account.CalculateBalance(1)
		unconfirmed := account.CalculateBalance(0) - balance
		NotifyWalletBalance(frontend, account.name, balance)
		NotifyWalletBalanceUnconfirmed(frontend, account.name, unconfirmed)
	}
}

// ListAccounts returns a map of account names to their current account
// balances.  The balances are calculated using minconf confirmations.
func (store *AccountStore) ListAccounts(minconf int) map[string]float64 {
	store.RLock()
	defer store.RUnlock()

	// Create and fill a map of account names and their balances.
	pairs := make(map[string]float64)
	for name, a := range store.accounts {
		pairs[name] = a.CalculateBalance(minconf)
	}
	return pairs
}

// ListSinceBlock returns a slice of maps of strings to interface containing
// structures defining all transactions in the wallets since the given block.
// To be used for the listsinceblock command.
func (store *AccountStore) ListSinceBlock(since, curBlockHeight int32, minconf int) ([]map[string]interface{}, error) {
	store.RLock()
	defer store.RUnlock()

	// Create and fill a map of account names and their balances.
	txInfoList := []map[string]interface{}{}
	for _, a := range store.accounts {
		txTmp, err := a.ListSinceBlock(since, curBlockHeight, minconf)
		if err != nil {
			return nil, err
		}
		txInfoList = append(txInfoList, txTmp...)
	}
	return txInfoList, nil
}

// RescanActiveAddresses begins a rescan for all active addresses for
// each account.
//
// TODO(jrick): batch addresses for all accounts together so multiple
// rescan commands can be avoided.
func (store *AccountStore) RescanActiveAddresses() {
	store.RLock()
	defer store.RUnlock()

	for _, account := range store.accounts {
		account.RescanActiveAddresses()
	}
}

// Track begins tracking all addresses in all accounts for updates from
// btcd.
func (store *AccountStore) Track() {
	store.RLock()
	defer store.RUnlock()

	for _, account := range store.accounts {
		account.Track()
	}
}

// WalletOpenError is a special error type so problems opening wallet
// files can be differentiated (by a type assertion) from other errors.
type WalletOpenError struct {
	Err string
}

// Error satisifies the builtin error interface.
func (e *WalletOpenError) Error() string {
	return e.Err
}

// OpenAccount opens an account described by account in the data
// directory specified by cfg.  If the wallet does not exist, ErrNoWallet
// is returned as an error.
//
// Wallets opened from this function are not set to track against a
// btcd connection.
func (store *AccountStore) OpenAccount(name string, cfg *config) error {
	wlt := new(wallet.Wallet)

	a := &Account{
		Wallet: wlt,
		name:   name,
	}

	netdir := networkDir(cfg.Net())
	if err := checkCreateDir(netdir); err != nil {
		return err
	}

	wfilepath := accountFilename("wallet.bin", name, netdir)
	utxofilepath := accountFilename("utxo.bin", name, netdir)
	txfilepath := accountFilename("tx.bin", name, netdir)
	var wfile, utxofile, txfile *os.File

	// Read wallet file.
	wfile, err := os.Open(wfilepath)
	if err != nil {
		if os.IsNotExist(err) {
			// Must create and save wallet first.
			return ErrNoWallet
		}
		msg := fmt.Sprintf("cannot open wallet file: %s", err)
		return &WalletOpenError{msg}
	}
	defer wfile.Close()

	if _, err = wlt.ReadFrom(wfile); err != nil {
		msg := fmt.Sprintf("cannot read wallet: %s", err)
		return &WalletOpenError{msg}
	}

	// Read tx file.  If this fails, return a ErrNoTxs error and let
	// the caller decide if a rescan is necessary.
	var finalErr error
	if txfile, err = os.Open(txfilepath); err != nil {
		log.Errorf("cannot open tx file: %s", err)
		// This is not a error we should immediately return with,
		// but other errors can be more important, so only return
		// this if none of the others are hit.
		finalErr = ErrNoTxs
	} else {
		defer txfile.Close()
		var txs tx.TxStore
		if _, err = txs.ReadFrom(txfile); err != nil {
			log.Errorf("cannot read tx file: %s", err)
			finalErr = ErrNoTxs
		} else {
			a.TxStore.s = txs
		}
	}

	// Read utxo file.  If this fails, return a ErrNoUtxos error so a
	// rescan can be done since the wallet creation block.
	var utxos tx.UtxoStore
	utxofile, err = os.Open(utxofilepath)
	if err != nil {
		log.Errorf("cannot open utxo file: %s", err)
		finalErr = ErrNoUtxos
	} else {
		defer utxofile.Close()
		if _, err = utxos.ReadFrom(utxofile); err != nil {
			log.Errorf("cannot read utxo file: %s", err)
			finalErr = ErrNoUtxos
		} else {
			a.UtxoStore.s = utxos
		}
	}

	store.Lock()
	switch finalErr {
	case ErrNoTxs:
		// Do nothing special for now.  This will be implemented when
		// the tx history file is properly written.
		store.accounts[name] = a

	case ErrNoUtxos:
		// Add wallet, but mark wallet as needing a full rescan since
		// the wallet creation block.  This will take place when btcd
		// connects.
		a.fullRescan = true
		store.accounts[name] = a
	case nil:
		store.accounts[name] = a

	default:
		log.Warnf("cannot open wallet: %v", err)
	}
	store.Unlock()

	// Mark all active payment addresses as belonging to this account.
	for addr := range a.ActivePaymentAddresses() {
		MarkAddressForAccount(addr, name)
	}

	return nil
}
