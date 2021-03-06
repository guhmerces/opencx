package cxserver

import (
	"fmt"

	"github.com/mit-dci/lit/coinparam"
	"github.com/mit-dci/lit/crypto/koblitz"
	"github.com/mit-dci/opencx/cxdb"
	"github.com/mit-dci/opencx/match"
)

// GetOrder gets the order for the given id from the limit orderbook
func (server *OpencxServer) GetOrder(orderID *match.OrderID) (order *match.LimitOrderIDPair, err error) {

	// We just go through everything, checking the limit orderbook, seeing if we get a match
	server.dbLock.Lock()
	for _, limBook := range server.Orderbooks {
		if order, err = limBook.GetOrder(orderID); err != nil && order == nil {
			err = fmt.Errorf("Error getting order from a limit orderbook: %s", err)
			server.dbLock.Unlock()
			return
		} else if err == nil && order != nil {
			server.dbLock.Unlock()
			return
		}
	}

	err = fmt.Errorf("Could not find order with that order ID")
	server.dbLock.Unlock()
	return
}

// PlaceOrder places an order by first checking if we can credit the user, then calling the appropriate
// database calls
func (server *OpencxServer) PlaceOrder(order *match.LimitOrder) (orderID *match.OrderID, err error) {

	var assetToCredit match.Asset
	// If we are buy then we want to credit assethave
	// If we are sell then we want to credit assetwant
	if order.Side == match.Buy {
		assetToCredit = order.TradingPair.AssetHave
	} else {
		assetToCredit = order.TradingPair.AssetWant
	}

	// just defensive programming here

	// if we can't turn the asset into coinparams then lol rip
	var param *coinparam.Params
	if param, err = assetToCredit.CoinParamFromAsset(); err != nil {
		err = fmt.Errorf("Could not turn order asset into coin param for PlaceOrder: %s", err)
		return
	}

	// make sure that putting it in the db will be an accurate and good idea because calculating prices is frustrating
	var pr float64
	if pr, err = order.Price(); err != nil {
		err = fmt.Errorf("Error calculating price while Placing: %s", err)
		return
	}

	// TODO: this is to protect the database, this is why switching to a better price system would be a good idea
	if pr > float64(10000000000000000000000) {
		err = fmt.Errorf("Price too high, complain online if you want the maximum price increased, or lower your price")
		return
	}
	if pr < float64(1)/float64(1000000) {
		err = fmt.Errorf("Price too low, complain online if you want the minimum price decreased, or increase your price")
		return
	}

	server.dbLock.Lock()

	// first we need to get the settlement engine, limit engine, orderbook, and settlement store
	var currSetEng match.SettlementEngine
	var ok bool
	if currSetEng, ok = server.SettlementEngines[param]; !ok {
		err = fmt.Errorf("Could not find correct settlement engine for PlaceOrder")
		server.dbLock.Unlock()
		return
	}

	var currMatchEng match.LimitEngine
	if currMatchEng, ok = server.MatchingEngines[order.TradingPair]; !ok {
		err = fmt.Errorf("Could not find matching engine for trading pair for PlaceOrder")
		server.dbLock.Unlock()
		return
	}

	var currOrderbook match.LimitOrderbook
	if currOrderbook, ok = server.Orderbooks[order.TradingPair]; !ok {
		err = fmt.Errorf("Could not find orderbooks for trading pair for PlaceOrder")
		server.dbLock.Unlock()
		return
	}

	var currSetStore cxdb.SettlementStore
	if currSetStore, ok = server.SettlementStores[param]; !ok {
		err = fmt.Errorf("Could not find settlement store for asset for PlaceOrder")
		server.dbLock.Unlock()
		return
	}

	orderCreditExec := &match.SettlementExecution{
		Pubkey: order.Pubkey,
		Type:   match.Credit,
		Asset:  assetToCredit,
		Amount: order.AmountHave,
	}
	// Let's hope that since they're both [33]byte their value can just be copied over through assignment
	// copy(orderCreditExec.Pubkey[:], order.Pubkey[:])

	// Okay now that we have these, check the validity
	var valid bool
	if valid, err = currSetEng.CheckValid(orderCreditExec); err != nil {
		err = fmt.Errorf("Error checking valid settlement exec: %s", err)
		server.dbLock.Unlock()
		return
	}

	if !valid {
		err = fmt.Errorf("Error placing order, not enough balance or you are not allowed to place orders")
		server.dbLock.Unlock()
		return
	}

	// Now we do these two operations. !!! IMPORTANT: THESE TWO CALLS NEED TO BE ATOMIC !!!
	// TODO: ensure atomicity. Currently the matching engine is the one thing that must either be
	// redundant or resistant to crashes / failure.

	// While at this point we can replay things, if the apply settlement execution succeeds
	// but the place limit order does not, then we'll have asymmetry.
	// One way to fix this is develop a clever fault-tolerant way of replaying these messages
	// if we detect a crash.

	// Long story short, distributed systems are hard.
	var settlementResults []*match.SettlementResult
	var setRes *match.SettlementResult
	if setRes, err = currSetEng.ApplySettlementExecution(orderCreditExec); err != nil {
		err = fmt.Errorf("Error applying settlement execution when placing order: %s", err)
		server.dbLock.Unlock()
		return
	}

	settlementResults = append(settlementResults, setRes)

	var idRes *match.LimitOrderIDPair
	if idRes, err = currMatchEng.PlaceLimitOrder(order); err != nil {
		err = fmt.Errorf("Error placing limit order for limit matching engine for PlaceOrder: %s", err)
		server.dbLock.Unlock()
		return
	}

	// This may not need to be atomic because we can rebuild the previous state using the messages
	// we have, we can worry less now about things crashing but should still worry

	var orderExecs []*match.OrderExecution
	var settlementExecs []*match.SettlementExecution
	if orderExecs, settlementExecs, err = currMatchEng.MatchLimitOrders(); err != nil {
		err = fmt.Errorf("Error matching orders for limit matching engine for PlaceOrder: %s", err)
		server.dbLock.Unlock()
		return
	}

	for _, setExec := range settlementExecs {

		var thisCoin *coinparam.Params
		if thisCoin, err = setExec.Asset.CoinParamFromAsset(); err != nil {
			err = fmt.Errorf("Error getting coin param from asset to find correct engine: %s", err)
			server.dbLock.Unlock()
			return
		}

		var thisAssetEngine match.SettlementEngine
		if thisAssetEngine, ok = server.SettlementEngines[thisCoin]; !ok {
			err = fmt.Errorf("Could not find correct settlement engine for PlaceOrder")
			server.dbLock.Unlock()
			return
		}

		// We reuse valid.
		if valid, err = thisAssetEngine.CheckValid(setExec); err != nil {
			err = fmt.Errorf("Error checking valid settlement exec after match for PlaceOrder: %s", err)
			server.dbLock.Unlock()
			return
		}

		if !valid {
			err = fmt.Errorf("Error with matching engine output settlement validity, exec: \n%s", setExec.String())
			server.dbLock.Unlock()
			return
		}

		// We reuse setRes.
		if setRes, err = thisAssetEngine.ApplySettlementExecution(setExec); err != nil {
			err = fmt.Errorf("Error applying settlement execution after match for PlaceOrder: %s", err)
			server.dbLock.Unlock()
			return
		}
		settlementResults = append(settlementResults, setRes)
	}

	// Now we don't worry any more. The matching engine and settlement engine have both responded.
	// If we needed to we could rebuild the state.

	// update orderbook
	if err = currOrderbook.UpdateBookPlace(idRes); err != nil {
		err = fmt.Errorf("Error placing order on orderbook for PlaceOrder: %s", err)
		server.dbLock.Unlock()
		return
	}

	for _, orderExec := range orderExecs {
		if err = currOrderbook.UpdateBookExec(orderExec); err != nil {
			err = fmt.Errorf("Error updating orderbook execution for PlaceOrder: %s", err)
			server.dbLock.Unlock()
			return
		}
	}

	// update what the client sees
	if err = currSetStore.UpdateBalances(settlementResults); err != nil {
		err = fmt.Errorf("Error updating balances with settlement results for PlaceOrder: %s", err)
		server.dbLock.Unlock()
		return
	}

	server.dbLock.Unlock()

	// Now we return thing
	orderID = idRes.OrderID
	return
}

// ViewOrderbook returns a view of the orderbook for the user
func (server *OpencxServer) ViewOrderbook(pair *match.Pair) (book map[float64][]*match.LimitOrderIDPair, err error) {

	server.dbLock.Lock()
	var currOrderbook match.LimitOrderbook
	var ok bool
	if currOrderbook, ok = server.Orderbooks[*pair]; !ok {
		err = fmt.Errorf("Could not find orderbooks for trading pair for ViewOrderbook")
		server.dbLock.Unlock()
		return
	}

	if book, err = currOrderbook.ViewLimitOrderBook(); err != nil {
		err = fmt.Errorf("Error viewing limit orderbook for server for ViewOrderbook: %s", err)
		server.dbLock.Unlock()
		return
	}
	server.dbLock.Unlock()

	return
}

// GetOrdersForPubkey returns orders for a specific pubkey and pair
func (server *OpencxServer) GetOrdersForPubkey(pubkey *koblitz.PublicKey) (orders []*match.LimitOrderIDPair, err error) {

	server.dbLock.Lock()
	var currOrderMap map[float64][]*match.LimitOrderIDPair
	for _, currOrderbook := range server.Orderbooks {
		// get the orders in map form
		// TODO: determine if the map return type of this API is really necessary
		if currOrderMap, err = currOrderbook.GetOrdersForPubkey(pubkey); err != nil {
			err = fmt.Errorf("Error getting book orders for pubkey for server GetOrdersForPubkey: %s", err)
			server.dbLock.Unlock()
			return
		}

		// now add them to the list
		for _, returnedOrders := range currOrderMap {
			orders = append(orders, returnedOrders...)
		}
	}
	server.dbLock.Unlock()

	return
}

// CancelOrder places an order by first checking if we can credit the user, then calling the appropriate
// database calls
func (server *OpencxServer) CancelOrder(order *match.LimitOrderIDPair) (err error) {

	var assetToDebit match.Asset
	// If we are buy then we want to credit assethave
	// If we are sell then we want to credit assetwant
	if order.Order.Side == match.Buy {
		assetToDebit = order.Order.TradingPair.AssetHave
	} else {
		assetToDebit = order.Order.TradingPair.AssetWant
	}

	// if we can't turn the asset into coinparams then lol rip
	var param *coinparam.Params
	if param, err = assetToDebit.CoinParamFromAsset(); err != nil {
		err = fmt.Errorf("Could not turn order asset into coin param for CancelOrder: %s", err)
		return
	}

	server.dbLock.Lock()

	// first we need to get the settlement engine, limit engine, orderbook, and settlement store
	var currSetEng match.SettlementEngine
	var ok bool
	if currSetEng, ok = server.SettlementEngines[param]; !ok {
		err = fmt.Errorf("Could not find correct settlement engine for CancelOrder")
		server.dbLock.Unlock()
		return
	}

	var currMatchEng match.LimitEngine
	if currMatchEng, ok = server.MatchingEngines[order.Order.TradingPair]; !ok {
		err = fmt.Errorf("Could not find matching engine for trading pair for CancelOrder")
		server.dbLock.Unlock()
		return
	}

	var currOrderbook match.LimitOrderbook
	if currOrderbook, ok = server.Orderbooks[order.Order.TradingPair]; !ok {
		err = fmt.Errorf("Could not find orderbooks for trading pair for CancelOrder")
		server.dbLock.Unlock()
		return
	}

	var currSetStore cxdb.SettlementStore
	if currSetStore, ok = server.SettlementStores[param]; !ok {
		err = fmt.Errorf("Could not find settlement store for asset for CancelOrder")
		server.dbLock.Unlock()
		return
	}

	// Now we do these two operations. !!! IMPORTANT: THESE TWO CALLS NEED TO BE ATOMIC !!!
	// TODO: ensure atomicity. Currently the matching engine is the one thing that must either be
	// redundant or resistant to crashes / failure.

	// While at this point we can replay things, if the apply settlement execution succeeds
	// but the place limit order does not, then we'll have asymmetry.
	// One way to fix this is develop a clever fault-tolerant way of replaying these messages
	// if we detect a crash.

	// Long story short, distributed systems are hard.
	var cancelled *match.CancelledOrder
	var cancelSettlement *match.SettlementExecution
	if cancelled, cancelSettlement, err = currMatchEng.CancelLimitOrder(order.OrderID); err != nil {
		err = fmt.Errorf("Error cancelling limit order for limit matching engine for CancelOrder: %s", err)
		server.dbLock.Unlock()
		return
	}

	var settlementExecs []*match.SettlementExecution
	settlementExecs = append(settlementExecs, cancelSettlement)
	var settlementResults []*match.SettlementResult
	var setRes *match.SettlementResult
	var valid bool
	for _, setExec := range settlementExecs {

		if valid, err = currSetEng.CheckValid(setExec); err != nil {
			err = fmt.Errorf("Error checking valid settlement exec after match for CancelOrder: %s", err)
			server.dbLock.Unlock()
			return
		}

		if !valid {
			err = fmt.Errorf("Error with matching engine output settlement validity, exec: \n%s", setExec.String())
			server.dbLock.Unlock()
			return
		}

		if setRes, err = currSetEng.ApplySettlementExecution(setExec); err != nil {
			err = fmt.Errorf("Error applying settlement execution after match for CancelOrder: %s", err)
			server.dbLock.Unlock()
			return
		}
		settlementResults = append(settlementResults, setRes)
	}

	// Now we don't worry any more. The matching engine and settlement engine have both responded.
	// If we needed to we could rebuild the state.

	// update orderbook
	if err = currOrderbook.UpdateBookCancel(cancelled); err != nil {
		err = fmt.Errorf("Error updating orderbook cancel for CancelOrder: %s", err)
		server.dbLock.Unlock()
		return
	}

	// update what the client sees
	if err = currSetStore.UpdateBalances(settlementResults); err != nil {
		err = fmt.Errorf("Error updating balances with settlement results for CancelOrder: %s", err)
		server.dbLock.Unlock()
		return
	}

	server.dbLock.Unlock()
	return
}
