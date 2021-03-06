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
	"github.com/conformal/btcscript"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwallet/tx"
	"github.com/conformal/btcwallet/wallet"
	"github.com/conformal/btcwire"
	"sort"
	"sync"
	"time"
)

// ErrInsufficientFunds represents an error where there are not enough
// funds from unspent tx outputs for a wallet to create a transaction.
var ErrInsufficientFunds = errors.New("insufficient funds")

// ErrUnknownBitcoinNet represents an error where the parsed or
// requested bitcoin network is invalid (neither mainnet nor testnet).
var ErrUnknownBitcoinNet = errors.New("unknown bitcoin network")

// ErrNonPositiveAmount represents an error where a bitcoin amount is
// not positive (either negative, or zero).
var ErrNonPositiveAmount = errors.New("amount is not positive")

// ErrNegativeFee represents an error where a fee is erroneously
// negative.
var ErrNegativeFee = errors.New("fee is negative")

// minTxFee is the default minimum transation fee (0.0001 BTC,
// measured in satoshis) added to transactions requiring a fee.
const minTxFee = 10000

// TxFeeIncrement represents the global transaction fee per KB of Tx
// added to newly-created transactions and sent as a reward to the block
// miner.  i is measured in satoshis.
var TxFeeIncrement = struct {
	sync.Mutex
	i int64
}{
	i: minTxFee,
}

// CreatedTx is a type holding information regarding a newly-created
// transaction, including the raw bytes, inputs, and an address and UTXO
// for change (if any).
type CreatedTx struct {
	rawTx      []byte
	txid       btcwire.ShaHash
	time       time.Time
	inputs     []*tx.Utxo
	outputs    []tx.Pair
	btcspent   int64
	fee        int64
	changeAddr *btcutil.AddressPubKeyHash
	changeUtxo *tx.Utxo
}

// TXID is a transaction hash identifying a transaction.
type TXID btcwire.ShaHash

// UnminedTXs holds a map of transaction IDs as keys mapping to a
// CreatedTx structure.  If sending a raw transaction succeeds, the
// tx is added to this map and checked again after each new block.
// If the new block contains a tx, it is removed from this map.
var UnminedTxs = struct {
	sync.Mutex
	m map[TXID]*CreatedTx
}{
	m: make(map[TXID]*CreatedTx),
}

// ByAmount defines the methods needed to satisify sort.Interface to
// sort a slice of Utxos by their amount.
type ByAmount []*tx.Utxo

func (u ByAmount) Len() int {
	return len(u)
}

func (u ByAmount) Less(i, j int) bool {
	return u[i].Amt < u[j].Amt
}

func (u ByAmount) Swap(i, j int) {
	u[i], u[j] = u[j], u[i]
}

// selectInputs selects the minimum number possible of unspent
// outputs to use to create a new transaction that spends amt satoshis.
// Previous outputs with less than minconf confirmations are ignored.  btcout
// is the total number of satoshis which would be spent by the combination
// of all selected previous outputs.  err will equal ErrInsufficientFunds if there
// are not enough unspent outputs to spend amt.
func selectInputs(s tx.UtxoStore, amt uint64, minconf int) (inputs []*tx.Utxo, btcout uint64, err error) {
	bs, err := GetCurBlock()
	if err != nil {
		return nil, 0, err
	}

	// Create list of eligible unspent previous outputs to use as tx
	// inputs, and sort by the amount in reverse order so a minimum number
	// of inputs is needed.
	eligible := make([]*tx.Utxo, 0, len(s))
	for _, utxo := range s {
		// TODO(jrick): if Height is -1, the UTXO is the result of spending
		// to a change address, resulting in a UTXO not yet mined in a block.
		// For now, disallow creating transactions until these UTXOs are mined
		// into a block and show up as part of the balance.
		if utxo.Height != -1 && int(bs.Height-utxo.Height) >= minconf {
			eligible = append(eligible, utxo)
		}
	}
	sort.Sort(sort.Reverse(ByAmount(eligible)))

	// Iterate throguh eligible transactions, appending to outputs and
	// increasing btcout.  This is finished when btcout is greater than the
	// requested amt to spend.
	for _, u := range eligible {
		inputs = append(inputs, u)
		if btcout += u.Amt; btcout >= amt {
			return inputs, btcout, nil
		}
	}
	if btcout < amt {
		return nil, 0, ErrInsufficientFunds
	}

	return inputs, btcout, nil
}

// txToPairs creates a raw transaction sending the amounts for each
// address/amount pair and fee to each address and the miner.  minconf
// specifies the minimum number of confirmations required before an
// unspent output is eligible for spending. Leftover input funds not sent
// to addr or as a fee for the miner are sent to a newly generated
// address. If change is needed to return funds back to an owned
// address, changeUtxo will point to a unconfirmed (height = -1, zeroed
// block hash) Utxo.  ErrInsufficientFunds is returned if there are not
// enough eligible unspent outputs to create the transaction.
func (a *Account) txToPairs(pairs map[string]int64, minconf int) (*CreatedTx, error) {
	// Recorded unspent transactions should not be modified until this
	// finishes.
	a.UtxoStore.RLock()
	defer a.UtxoStore.RUnlock()

	// Create a new transaction which will include all input scripts.
	msgtx := btcwire.NewMsgTx()

	// Calculate minimum amount needed for inputs.
	var amt int64
	for _, v := range pairs {
		// Error out if any amount is negative.
		if v <= 0 {
			return nil, ErrNonPositiveAmount
		}
		amt += v
	}

	// outputs is a tx.Pair slice representing each output that is created
	// by the transaction.
	outputs := make([]tx.Pair, 0, len(pairs)+1)

	// Add outputs to new tx.
	for addrStr, amt := range pairs {
		addr, err := btcutil.DecodeAddr(addrStr)
		if err != nil {
			return nil, fmt.Errorf("cannot decode address: %s", err)
		}

		// Add output to spend amt to addr.
		pkScript, err := btcscript.PayToAddrScript(addr)
		if err != nil {
			return nil, fmt.Errorf("cannot create txout script: %s", err)
		}
		txout := btcwire.NewTxOut(int64(amt), pkScript)
		msgtx.AddTxOut(txout)

		// Create amount, address pair and add to outputs.
		out := tx.Pair{
			Amount:     amt,
			PubkeyHash: addr.ScriptAddress(),
		}
		outputs = append(outputs, out)
	}

	// Get current block's height and hash.
	bs, err := GetCurBlock()
	if err != nil {
		return nil, err
	}

	// Make a copy of msgtx before any inputs are added.  This will be
	// used as a starting point when trying a fee and starting over with
	// a higher fee if not enough was originally chosen.
	txNoInputs := msgtx.Copy()

	// These are nil/zeroed until a change address is needed, and reused
	// again in case a change utxo has already been chosen.
	var changeAddr *btcutil.AddressPubKeyHash

	var btcspent int64
	var selectedInputs []*tx.Utxo
	var finalChangeUtxo *tx.Utxo

	// Get the number of satoshis to increment fee by when searching for
	// the minimum tx fee needed.
	var fee int64 = 0
	for {
		msgtx = txNoInputs.Copy()

		// Select unspent outputs to be used in transaction based on the amount
		// neededing to sent, and the current fee estimation.
		inputs, btcin, err := selectInputs(a.UtxoStore.s, uint64(amt+fee),
			minconf)
		if err != nil {
			return nil, err
		}

		// Check if there are leftover unspent outputs, and return coins back to
		// a new address we own.
		var changeUtxo *tx.Utxo
		change := btcin - uint64(amt+fee)
		if change > 0 {
			// Create a new address to spend leftover outputs to.

			// Get a new change address if one has not already been found.
			if changeAddr == nil {
				changeAddr, err = a.NextChainedAddress(&bs, cfg.KeypoolSize)
				if err != nil {
					return nil, fmt.Errorf("failed to get next address: %s", err)
				}

				// Mark change address as belonging to this account.
				MarkAddressForAccount(changeAddr.EncodeAddress(), a.Name())
			}

			// Spend change.
			pkScript, err := btcscript.PayToAddrScript(changeAddr)
			if err != nil {
				return nil, fmt.Errorf("cannot create txout script: %s", err)
			}
			msgtx.AddTxOut(btcwire.NewTxOut(int64(change), pkScript))

			changeUtxo = &tx.Utxo{
				Amt: change,
				Out: tx.OutPoint{
					// Hash is unset (zeroed) here and must be filled in
					// with the transaction hash of the complete
					// transaction.
					Index: uint32(len(pairs)),
				},
				Height:    -1,
				Subscript: pkScript,
			}
			copy(changeUtxo.AddrHash[:], changeAddr.ScriptAddress())
		}

		// Selected unspent outputs become new transaction's inputs.
		for _, ip := range inputs {
			msgtx.AddTxIn(btcwire.NewTxIn((*btcwire.OutPoint)(&ip.Out), nil))
		}
		for i, ip := range inputs {
			// Error is ignored as the length and network checks can never fail
			// for these inputs.
			addr, _ := btcutil.NewAddressPubKeyHash(ip.AddrHash[:],
				a.Wallet.Net())
			privkey, err := a.AddressKey(addr)
			if err == wallet.ErrWalletLocked {
				return nil, wallet.ErrWalletLocked
			} else if err != nil {
				return nil, fmt.Errorf("cannot get address key: %v", err)
			}
			ai, err := a.AddressInfo(addr)
			if err != nil {
				return nil, fmt.Errorf("cannot get address info: %v", err)
			}

			sigscript, err := btcscript.SignatureScript(msgtx, i,
				ip.Subscript, btcscript.SigHashAll, privkey,
				ai.Compressed)
			if err != nil {
				return nil, fmt.Errorf("cannot create sigscript: %s", err)
			}
			msgtx.TxIn[i].SignatureScript = sigscript
		}

		noFeeAllowed := false
		if !cfg.DisallowFree {
			noFeeAllowed = allowFree(bs.Height, inputs, msgtx.SerializeSize())
		}
		if minFee := minimumFee(msgtx, noFeeAllowed); fee < minFee {
			fee = minFee
		} else {
			// Fill Tx hash of change outpoint with transaction hash.
			if changeUtxo != nil {
				txHash, err := msgtx.TxSha()
				if err != nil {
					return nil, fmt.Errorf("cannot create transaction hash: %s", err)
				}
				copy(changeUtxo.Out.Hash[:], txHash[:])

				// Add change to outputs.
				out := tx.Pair{
					Amount:     int64(change),
					PubkeyHash: changeAddr.ScriptAddress(),
					Change:     true,
				}
				outputs = append(outputs, out)

				finalChangeUtxo = changeUtxo
			}

			selectedInputs = inputs

			btcspent = int64(btcin)

			break
		}
	}

	// Validate msgtx before returning the raw transaction.
	flags := btcscript.ScriptCanonicalSignatures
	bip16 := time.Now().After(btcscript.Bip16Activation)
	if bip16 {
		flags |= btcscript.ScriptBip16
	}
	for i, txin := range msgtx.TxIn {
		engine, err := btcscript.NewScript(txin.SignatureScript,
			selectedInputs[i].Subscript, i, msgtx, flags)
		if err != nil {
			return nil, fmt.Errorf("cannot create script engine: %s", err)
		}
		if err = engine.Execute(); err != nil {
			return nil, fmt.Errorf("cannot validate transaction: %s", err)
		}
	}

	txid, err := msgtx.TxSha()
	if err != nil {
		return nil, fmt.Errorf("cannot create txid for created tx: %v", err)
	}

	buf := new(bytes.Buffer)
	msgtx.BtcEncode(buf, btcwire.ProtocolVersion)
	info := &CreatedTx{
		rawTx:      buf.Bytes(),
		txid:       txid,
		time:       time.Now(),
		inputs:     selectedInputs,
		outputs:    outputs,
		btcspent:   btcspent,
		fee:        fee,
		changeAddr: changeAddr,
		changeUtxo: finalChangeUtxo,
	}
	return info, nil
}

// minimumFee calculates the minimum fee required for a transaction.
// If allowFree is true, a fee may be zero so long as the entire
// transaction has a serialized length less than 1 kilobyte
// and none of the outputs contain a value less than 1 bitcent.
// Otherwise, the fee will be calculated using TxFeeIncrement,
// incrementing the fee for each kilobyte of transaction.
func minimumFee(tx *btcwire.MsgTx, allowFree bool) int64 {
	txLen := tx.SerializeSize()
	TxFeeIncrement.Lock()
	incr := TxFeeIncrement.i
	TxFeeIncrement.Unlock()
	fee := int64(1+txLen/1000) * incr

	if allowFree && txLen < 1000 {
		fee = 0
	}

	if fee < incr {
		for _, txOut := range tx.TxOut {
			if txOut.Value < btcutil.SatoshiPerBitcent {
				return incr
			}
		}
	}

	if fee < 0 || fee > btcutil.MaxSatoshi {
		fee = btcutil.MaxSatoshi
	}

	return fee
}

// allowFree calculates the transaction priority and checks that the
// priority reaches a certain threshhold.  If the threshhold is
// reached, a free transaction fee is allowed.
func allowFree(curHeight int32, inputs []*tx.Utxo, txSize int) bool {
	const blocksPerDayEstimate = 144
	const txSizeEstimate = 250

	var weightedSum int64
	for _, utxo := range inputs {
		depth := chainDepth(utxo.Height, curHeight)
		weightedSum += int64(utxo.Amt) * int64(depth)
	}
	priority := float64(weightedSum) / float64(txSize)
	return priority > float64(btcutil.SatoshiPerBitcoin)*blocksPerDayEstimate/txSizeEstimate
}

// chainDepth returns the chaindepth of a target given the current
// blockchain height.
func chainDepth(target, current int32) int32 {
	if target == -1 {
		// target is not yet in a block.
		return 0
	}

	// target is in a block.
	return current - target + 1
}
