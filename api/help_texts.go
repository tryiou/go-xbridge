package api

// helpCommandNames lists every supported command for the bare help output,
// sorted. Kept explicit (not ranged from dispatch) because a handler in the
// dispatch table cannot reference the table itself; TestDxHelpBare enforces
// set-equality with the dispatch keys.
var helpCommandNames = []string{
	"dxCancelOrder",
	"dxFlushCancelledOrders",
	"dxGetLocalTokens",
	"dxGetLockedUtxos",
	"dxGetMyOrders",
	"dxGetMyPartialOrderChain",
	"dxGetNetworkTokens",
	"dxGetNewTokenAddress",
	"dxGetOrder",
	"dxGetOrderBook",
	"dxGetOrderFills",
	"dxGetOrderHistory",
	"dxGetOrders",
	"dxGetTokenBalances",
	"dxGetTradingData",
	"dxGetUtxos",
	"dxLoadXBridgeConf",
	"dxMakeOrder",
	"dxMakePartialOrder",
	"dxPartialOrderChainDetails",
	"dxSplitAddress",
	"dxSplitInputs",
	"dxTakeOrder",
	"getnetworkinfo",
	"help",
}

// helpByMethod maps every supported command to its RPCHelpMan text. The
// help command appends Core's trailing newline; the arity-throw path uses
// the bare text.
var helpByMethod = map[string]string{
	"dxMakeOrder":                helpDxMakeOrder,
	"dxMakePartialOrder":         helpDxMakePartialOrder,
	"dxTakeOrder":                helpDxTakeOrder,
	"dxGetMyPartialOrderChain":   helpDxGetMyPartialOrderChain,
	"dxPartialOrderChainDetails": helpDxPartialOrderChainDetails,
	"dxSplitAddress":             helpDxSplitAddress,
	"dxSplitInputs":              helpDxSplitInputs,
	"dxGetUtxos":                 helpDxGetUtxos,
	"dxGetTradingData":           helpDxGetTradingData,
	"dxGetOrders":                helpDxGetOrders,
	"dxGetOrder":                 helpDxGetOrder,
	"dxGetOrderBook":             helpDxGetOrderBook,
	"dxGetOrderFills":            helpDxGetOrderFills,
	"dxGetOrderHistory":          helpDxGetOrderHistory,
	"dxGetTokenBalances":         helpDxGetTokenBalances,
	"dxGetLocalTokens":           helpDxGetLocalTokens,
	"dxGetNetworkTokens":         helpDxGetNetworkTokens,
	"dxGetNewTokenAddress":       helpDxGetNewTokenAddress,
	"dxGetLockedUtxos":           helpDxGetLockedUtxos,
	"dxCancelOrder":              helpDxCancelOrder,
	"dxFlushCancelledOrders":     helpDxFlushCancelledOrders,
	"dxLoadXBridgeConf":          helpDxLoadXBridgeConf,
	"dxGetMyOrders":              helpDxGetMyOrders,
	"getnetworkinfo":             helpGetnetworkinfo,
	"help":                       helpDxHelp,
}

// Transcribed from Blocknet Core 4.4.1 src/xbridge/rpcxbridge.cpp via a
// standalone reproduction of the 0.18 RPCHelpMan::ToString() rendering
// (src/rpc/util.cpp). These are the exact byte strings thrown as the envelope
// error code -1 message when a throw-method arity gate fails (rpc/server.cpp:584-586).
const (
	helpDxMakeOrder        = "dxMakeOrder \"maker\" \"maker_size\" \"maker_address\" \"taker\" \"taker_size\" \"taker_address\" \"type\" ( use_all_funds \"dryrun\" )\n\nCreate a new exact order. Exact orders must be taken for the full order amount. For partial orders, see dxMakePartialOrder.\nYou can only create orders for markets with assets supported by your node (view with dxGetLocalTokens) and the network (view with dxGetNetworkTokens). There are no fees to make orders.\n\nNote:\nXBridge will first attempt to use funds from the specified maker address. If this address does not have sufficient funds to cover the order and `use_all_funds` is true, then it will pull funds from other addresses in the wallet. Change is deposited to the address with the largest input used.\n\nArguments:\n1. maker            (string, required) The symbol of the asset being sold by the maker (e.g. LTC).\n2. maker_size       (string, required) The amount of the maker asset being sent.\n3. maker_address    (string, required) The maker address containing asset being sent.\n4. taker            (string, required) The symbol of the asset being bought by the maker (e.g. BLOCK).\n5. taker_size       (string, required) The amount of the taker asset to be received.\n6. taker_address    (string, required) The taker address for the receiving asset.\n7. type             (string, required) The order type. Options: exact\n8. use_all_funds    (boolean, optional, default=true) Use funds from all available addresses in the wallet as opposed to just the maker_address.\n9. dryrun           (string) Simulate the order submission without actually submitting the order, i.e. a test run. Options: dryrun\n\nResult:\n\n    {\n        \"id\": \"4306a107113c4562afa6273ecd9a3990ead53a0227f74ddd9122272e453ae07d\",\n        \"maker\": \"SYS\",\n        \"maker_size\": \"1.000000\",\n        \"maker_address\": \"SVTbaYZ8olpVn3uNyImst3GKyrvfzXQgdK\",\n        \"taker\": \"LTC\",\n        \"taker_size\": \"0.100000\",\n        \"taker_address\": \"LVvFhZroMRGTtg1hHp7jVew3YoZRX8y35Z\",\n        \"updated_at\": \"2018-01-16T00:00:00.00000Z\",\n        \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n        \"block_id\": \"38729344720548447445023782734923740427863289632489723984723\",\n        \"order_type\": \"exact\",\n        \"partial_minimum\": \"0.000000\",\n        \"partial_orig_maker_size\": \"0.000000\",\n        \"partial_orig_taker_size\": \"0.000000\",\n        \"partial_repost\": false,\n        \"partial_parent_id\": \"\",\n        \"status\": \"created\"\n    }\n\n    Key                     | Type | Description\n    ------------------------|------|---------------------------------------------\n    Array                   | arr  | An array of all orders with each order\n                            |      | having the following parameters.\n    id                      | str  | The order ID.\n    maker                   | str  | Maker trading asset; the ticker of the asset\n                            |      | being sold by the maker.\n    maker_size              | str  | Maker trading size. String is used to\n                            |      | preserve precision.\n    maker_address           | str  | Address for sending the outgoing asset.\n    taker                   | str  | Taker trading asset; the ticker of the asset\n                            |      | being sold by the taker.\n    taker_size              | str  | Taker trading size. String is used to\n                            |      | preserve precision.\n    taker_address           | str  | Address for receiving the incoming asset.\n    updated_at              | str  | ISO 8601 datetime, with microseconds, of the\n                            |      | last time the order was updated.\n    created_at              | str  | ISO 8601 datetime, with microseconds, of\n                            |      | when the order was created.\n    order_type              | str  | The order type.\n    partial_minimum*        | str  | The minimum amount that can be taken.\n    partial_orig_maker_size*| str  | The partial order original maker_size.\n    partial_orig_taker_size*| str  | The partial order original taker_size.\n    partial_repost          | str  | Whether the order will be reposted or not.\n                            |      | This applies to `partial` order types and\n                            |      | will show `false` for `exact` order types.\n    partial_parent_id       | str  | The previous order id of a reposted partial\n                            |      | order. This will return an empty string if\n                            |      | there is no parent order.\n    status                  | str  | The order status.\n\n    * This only applies to `partial` order types and will show `0` on `exact`\n      order types.\n                \nExamples:\n> blocknet-cli dxMakeOrder LTC 25 LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H BLOCK 1000 BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR exact\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxMakeOrder\", \"params\": [\"LTC\", \"25\", \"LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H\", \"BLOCK\", \"1000\", \"BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR\", \"exact\"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/\n> blocknet-cli dxMakeOrder LTC 25 LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H BLOCK 1000 BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR exact true dryrun\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxMakeOrder\", \"params\": [\"LTC\", \"25\", \"LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H\", \"BLOCK\", \"1000\", \"BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR\", \"exact\", \"true\", \"dryrun\"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpDxMakePartialOrder = "dxMakePartialOrder \"maker\" \"maker_size\" \"maker_address\" \"taker\" \"taker_size\" \"taker_address\" \"minimum_size\" ( repost use_all_funds auto_split \"dryrun\" )\n\nCreate a new partial order. Partial orders don't require the entire order to be filled. For exact orders, see dxMakeOrder.\nYou can only create orders for markets with assets supported by your node (view with dxGetLocalTokens) and the network (view with dxGetNetworkTokens). There are no fees to make orders. \n\nWhen a partial order is created, multiple inputs will be selected or generated. Using multiple inputs is optimal for allowing partial orders of varying sizes while minimizing the amount of change (change not reposted). This maximizes the amount remaining that can be immediately reposted.\n\nThe way input selection/generation is done depends on your total `maker_size` and `minimum_size`. XBridge will first attempt to find existing inputs that are properly sized for the order. If needed, existing inputs will automatically be split into the proper size at the time the order is posted. While the inputs are being generated, the order will remain in the `new` state. Once the generated inputs have 1 confirmation the order will proceed to the `open` state.\n\nNote:\nXBridge will first attempt to use funds from the specified maker address. If this address does not have sufficient funds to cover the order and `use_all_funds` is true, then it will pull funds from other addresses in the wallet. Change is deposited to the address with the largest input used.\n\nArguments:\n1. maker            (string, required) The symbol of the asset being sold by the maker (e.g. LTC).\n2. maker_size       (string, required) The amount of the maker asset being sent.\n3. maker_address    (string, required) The maker address containing asset being sent.\n4. taker            (string, required) The symbol of the asset being bought by the maker (e.g. BLOCK).\n5. taker_size       (string, required) The amount of the taker asset to be received.\n6. taker_address    (string, required) The taker address for the receiving asset.\n7. minimum_size     (string, required) Minimum maker_size that can be traded in the partial order.\n8. repost           (boolean, optional, default=true) Repost partial order remainder after taken.\n9. use_all_funds    (boolean, optional, default=true) Use funds from all available addresses in the wallet as opposed to just the maker_address.\n10. auto_split      (boolean, optional, default=true) Split funds into multiple UTXOs if needed.\n11. dryrun          (string) Simulate the order submission without actually submitting the order, i.e. a test run. Options: dryrun\n\nResult:\n\n    {\n        \"id\": \"4306a107113c4562afa6273ecd9a3990ead53a0227f74ddd9122272e453ae07d\",\n        \"maker\": \"SYS\",\n        \"maker_size\": \"1.000000\",\n        \"maker_address\": \"SVTbaYZ8olpVn3uNyImst3GKyrvfzXQgdK\",\n        \"taker\": \"LTC\",\n        \"taker_size\": \"0.100000\",\n        \"taker_address\": \"LVvFhZroMRGTtg1hHp7jVew3YoZRX8y35Z\",\n        \"updated_at\": \"2018-01-16T00:00:00.00000Z\",\n        \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n        \"block_id\": \"38729344720548447445023782734923740427863289632489723984723\",\n        \"order_type\": \"partial\",\n        \"partial_minimum\": \"0.200000\",\n        \"partial_orig_maker_size\": \"2.000000\",\n        \"partial_orig_taker_size\": \"0.200000\",\n        \"partial_repost\": true,\n        \"partial_parent_id\": \"1faeba06827929f16490c61ba633522158e8d44163c47f735078eac0304c5eb6\",\n        \"status\": \"created\"\n    }\n\n    Key                     | Type | Description\n    ------------------------|------|---------------------------------------------\n    Array                   | arr  | An array of all orders with each order\n                            |      | having the following parameters.\n    id                      | str  | The order ID.\n    maker                   | str  | Maker trading asset; the ticker of the asset\n                            |      | being sold by the maker.\n    maker_size              | str  | Maker trading size. String is used to\n                            |      | preserve precision.\n    maker_address           | str  | Address for sending the outgoing asset.\n    taker                   | str  | Taker trading asset; the ticker of the asset\n                            |      | being sold by the taker.\n    taker_size              | str  | Taker trading size. String is used to\n                            |      | preserve precision.\n    taker_address           | str  | Address for receiving the incoming asset.\n    updated_at              | str  | ISO 8601 datetime, with microseconds, of the\n                            |      | last time the order was updated.\n    created_at              | str  | ISO 8601 datetime, with microseconds, of\n                            |      | when the order was created.\n    order_type              | str  | The order type.\n    partial_minimum*        | str  | The minimum amount that can be taken.\n    partial_orig_maker_size*| str  | The partial order original maker_size.\n    partial_orig_taker_size*| str  | The partial order original taker_size.\n    partial_repost          | str  | Whether the order will be reposted or not.\n                            |      | This applies to `partial` order types and\n                            |      | will show `false` for `exact` order types.\n    partial_parent_id       | str  | The previous order id of a reposted partial\n                            |      | order. This will return an empty string if\n                            |      | there is no parent order.\n    status                  | str  | The order status.\n\n    * This only applies to `partial` order types and will show `0` on `exact`\n      order types.\n                \nExamples:\n> blocknet-cli dxMakePartialOrder LTC 25 LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H BLOCK 1000 BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR 100\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxMakePartialOrder\", \"params\": [\"LTC\", \"25\", \"LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H\", \"BLOCK\", \"1000\", \"BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR\", \"100\"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/\n> blocknet-cli dxMakePartialOrder LTC 25 LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H BLOCK 1000 BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR 100 true true true dryrun\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxMakePartialOrder\", \"params\": [\"LTC\", \"25\", \"LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H\", \"BLOCK\", \"1000\", \"BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR\", \"100\", \"true\", \"true\", \"true\", \"dryrun\"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpDxTakeOrder        = `dxTakeOrder "id" "from_address" "to_address" ( "amount" "dryrun" )

This call is used to take an order. You can only take orders for assets supported by your node (view with dxGetLocalTokens). Taking your own order is not supported. Taking an order has a 0.015 BLOCK fee.

Note:
XBridge will first attempt to use funds from the specified from_address. If this address does not have sufficient funds to cover the order, then it will pull funds from other addresses in the wallet. Change is deposited to the address with the largest input used.

Arguments:
1. id              (string, required) The ID of the order being filled.
2. from_address    (string, required) The address containing asset being sent.
3. to_address      (string, required) The address for the receiving asset.
4. amount          (string) The amount to take (allowed only on partial orders)
5. dryrun          (string) Simulate the order submission without actually submitting the order, i.e. a test run. Options: dryrun

Result:

    {
        "id": "4306aa07113c4562ffa6278ecd9a3990ead53a0227f74ddd9122272e453ae07d",
        "maker": "SYS",
        "maker_size": "0.100",
        "taker": "LTC",
        "taker_size": "0.01",
        "updated_at": "1970-01-01T00:00:00.00000Z",
        "created_at": "2018-01-15T18:15:30.12345Z",
        "order_type": "exact",
        "partial_minimum": "0.000000",
        "partial_repost": false,
        "status": "accepting"
    }

    Key             | Type | Description
    ----------------|------|-----------------------------------------------------
    id              | str  | The order ID.
    maker           | str  | Maker trading asset; the ticker of the asset being
                    |      | sold by the maker.
    maker_size      | str  | Maker trading size. String is used to preserve
                    |      | precision.
    taker           | str  | Taker trading asset; the ticker of the asset being
                    |      | sold by the taker.
    taker_size      | str  | Taker trading size. String is used to preserve
                    |      | precision.
    updated_at      | str  | ISO 8601 datetime, with microseconds, of the last
                    |      | time the order was updated.
    created_at      | str  | ISO 8601 datetime, with microseconds, of when the
                    |      | order was created.
    status          | str  | The order status.
                
Examples:
> blocknet-cli dxTakeOrder 524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432 LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxTakeOrder", "params": ["524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432", "LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H", "BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxTakeOrder 524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432 LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR 0.5
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxTakeOrder", "params": ["524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432", "LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H", "BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR", "0.5"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxTakeOrder 524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432 LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR 0.5 dryrun
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxTakeOrder", "params": ["524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432", "LLZ1pgb6Jqx8hu84fcr5WC5HMoKRUsRE8H", "BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR", "0.5", "dryrun"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetMyPartialOrderChain   = "dxGetMyPartialOrderChain \"order_id\"\n\nReturns a list of all orders related to the specified order id. This includes partial orders that were repost from a parent order.\n\nArguments:\n1. order_id    (string, required) Order id\n\nResult:\n\n    [\n        {\n            \"id\": \"91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9\",\n            \"maker\": \"SYS\",\n            \"maker_size\": \"100.000000\",\n            \"maker_address\": \"SVTbaYZ8olpVn3uNyImst3GKyrvfzXQgdK\",\n            \"taker\": \"LTC\",\n            \"taker_size\": \"10.500000\",\n            \"taker_address\": \"LVvFhZroMRGTtg1hHp7jVew3YoZRX8y35Z\",\n            \"updated_at\": \"2018-01-15T18:25:05.12345Z\",\n            \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n            \"order_type\": \"partial\",\n            \"partial_minimum\": \"10.000000\",\n            \"partial_orig_maker_size\": \"100.000000\",\n            \"partial_orig_taker_size\": \"10.500000\",\n            \"partial_repost\": true,\n            \"partial_parent_id\": \"\",\n            \"status\": \"open\"\n        },\n        {\n            \"id\": \"6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a\",\n            \"maker\": \"SYS\",\n            \"maker_size\": \"4.000000\",\n            \"maker_address\": \"SVTbaYZ8olpVn3uNyImst3GKyrvfzXQgdK\",\n            \"taker\": \"LTC\",\n            \"taker_size\": \"0.400000\",\n            \"taker_address\": \"LVvFhZroMRGTtg1hHp7jVew3YoZRX8y35Z\",\n            \"updated_at\": \"2018-01-15T18:25:05.12345Z\",\n            \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n            \"order_type\": \"partial\",\n            \"partial_minimum\": \"0.400000\",\n            \"partial_orig_maker_size\": \"4.000000\",\n            \"partial_orig_taker_size\": \"0.400000\",\n            \"partial_repost\": true,\n            \"partial_parent_id\": \"91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9\",\n            \"status\": \"open\"\n        }\n    ]\n\n    Key                     | Type | Description\n    ------------------------|------|---------------------------------------------\n    Array                   | arr  | An array of all orders with each order\n                            |      | having the following parameters.\n    id                      | str  | The order ID.\n    maker                   | str  | Maker trading asset; the ticker of the asset\n                            |      | being sold by the maker.\n    maker_size              | str  | Maker trading size. String is used to\n                            |      | preserve precision.\n    maker_address           | str  | Address for sending the outgoing asset.\n    taker                   | str  | Taker trading asset; the ticker of the asset\n                            |      | being sold by the taker.\n    taker_size              | str  | Taker trading size. String is used to\n                            |      | preserve precision.\n    taker_address           | str  | Address for receiving the incoming asset.\n    updated_at              | str  | ISO 8601 datetime, with microseconds, of the\n                            |      | last time the order was updated.\n    created_at              | str  | ISO 8601 datetime, with microseconds, of\n                            |      | when the order was created.\n    order_type              | str  | The order type.\n    partial_minimum*        | str  | The minimum amount that can be taken.\n    partial_orig_maker_size*| str  | The partial order original maker_size.\n    partial_orig_taker_size*| str  | The partial order original taker_size.\n    partial_repost          | str  | Whether the order will be reposted or not.\n                            |      | This applies to `partial` order types and\n                            |      | will show `false` for `exact` order types.\n    partial_parent_id       | str  | The previous order id of a reposted partial\n                            |      | order. This will return an empty string if\n                            |      | there is no parent order.\n    status                  | str  | The order status.\n\n    * This only applies to `partial` order types and will show `0` on `exact`\n      order types.\n                \nExamples:\n> blocknet-cli dxGetMyPartialOrderChain \"6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a\"\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxGetMyPartialOrderChain\", \"params\": [6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpDxPartialOrderChainDetails = "dxPartialOrderChainDetails \"order_id\"\n\nReturns detailed information about a partial order chain. This includes original amounts, total amount, reported amounts sent and received and other information.\n\nArguments:\n1. order_id    (string, required) Order id\n\nResult:\n\n    {\n        \"first_order_id\": \"0b28e7c7de9a048dd2cb28b7d91062a052d16adf6d1a2154aa99ab2321c29770\",\n        \"maker\": \"BLOCK\",\n        \"maker_address\": \"y4Fn5z58KFA4qLcktBFCrKc8UHrWnNaVym\",\n        \"taker\": \"LTC\",\n        \"taker_address\": \"LWvt2ygq8QDkVEcCkMWHR4qXCqL2gC9D2B\",\n        \"partial_minimum\": \"0.100000\",\n        \"partial_orig_maker_size\": \"0.100000\",\n        \"partial_orig_taker_size\": \"0.000100\",\n        \"first_order_time\": \"2020-07-23T23:52:05.999Z\",\n        \"last_order_time\": \"2020-07-23T23:58:34.604Z\",\n        \"total_reported_sent\": \"0.200000\",\n        \"total_reported_received\": \"0.000200\",\n        \"total_reported_notsent\": \"0.800000\",\n        \"total_reported_notreceived\": \"0.000800\",\n        \"total_orders_open\": 0,\n        \"total_orders_finished\": 2,\n        \"total_orders_canceled\": 1,\n        \"orders\": [\n          \"0b28e7c7de9a048dd2cb28b7d91062a052d16adf6d1a2154aa99ab2321c29770\",\n          \"d3afd3b5faf604245a6962214bd0460bec88ff275236480d24b9e5cd45d44c41\",\n          \"5d4bde2de3d6982ce40da82da3b55803f82e11672b2292c611aec9b54cc4c4c9\"\n        ],\n        \"p2sh_deposits\": [\n          \"a3bd9b849696946a06ad90b5e03337dba326400192d5b9b96ce0faf2cb513377\",\n          \"a29c4d06941877b501d0b5fe6dc054ca177f723e30a987f67a5871df8b14bfa5\",\n          \"\"\n        ],\n        \"p2sh_deposits_counterparty\": [\n          \"41e106c3668d097166cc4a5cce283a9079e769859c4a5467826506fc2547725e\",\n          \"c2f86465d26b3f90f559e3fe56a4a0aa44ee01e07f1e27b2236f16be92991f25\",\n          \"\"\n        ]\n    }\n\n    Key                        | Type | Description\n    ---------------------------|------|-----------------------------------------------------\n    first_order_id             | str  | The order ID.\n    maker                      | str  | Maker trading asset; the ticker of the asset being\n                               |      | sold by the maker.\n    maker_address              | str  | Address for sending the outgoing asset.\n    taker                      | str  | Taker trading asset; the ticker of the asset being\n                               |      | sold by the taker.\n    taker_address              | str  | Address for receiving the incoming asset.\n    partial_minimum            | str  | The minimum amount that can be taken. This applies\n                               |      | to `partial` order types and will show `0` on\n                               |      | `exact` order types.\n    partial_orig_maker_size    | str  | The partial order original maker_size.\n    partial_orig_taker_size    | str  | The partial order original taker_size.\n    first_order_time           | str  | ISO 8601 datetime, with microseconds, of the last\n                               |      | time the order was updated.\n    last_order_time            | str  | ISO 8601 datetime, with microseconds, of when the\n                               |      | order was created.\n    total_reported_sent        | str  | Total amount of maker coin sent to traders.\n    total_reported_received    | str  | Total amount of taker coin received from traders.\n    total_reported_notsent     | str  | Total amount of maker coin not yet sent to traders.\n    total_reported_notreceived | str  | Total amount of taker coin not yet received from traders.\n    total_orders_open          | int  | Total number of open orders.\n    total_orders_finished      | int  | Total number of completed orders.\n    total_orders_canceled      | int  | Total number of canceled orders.\n    orders                     | arr  | All orders in the partial order chain.\n    p2sh_deposits              | arr  | All p2sh deposit txids sorted by \"orders\" data (1 for each order)\n    p2sh_deposits_counterparty | arr  | All p2sh counterparty deposit txids sorted by \"orders\" data (1 for each order)\n\n                \nExamples:\n> blocknet-cli dxPartialOrderChainDetails \"6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a\"\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxPartialOrderChainDetails\", \"params\": [6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpDxSplitAddress             = "dxSplitAddress \"token\" \"split_amount\" \"address\" ( include_fees show_rawtx submit )\n\nSplits unused coin in the given address into the specified size. Left over amounts end up in change. UTXOs being used in existing orders will not be included by the splitter (view with dxGetUtxos). You can only split UTXOs for assets supported by your node (view with dxGetLocalTokens).\n\nArguments:\n1. token           (string, required) The ticker of the asset you want to split UTXOs for.\n2. split_amount    (string, required) The desired UTXO output size.\n3. address         (string, required) The address to split UTXOs in. Only coin in this address will be split.\n4. include_fees    (boolean, optional, default=true) Include the trade P2SH deposit fees in the split UTXO (add deposit fee to `spit_amount` value.\n5. show_rawtx      (boolean, optional, default=false) Include the raw transaction in the response (rawtx can be submitted manually).\n6. submit          (boolean, optional, default=true) Submit the raw transaction to the network.\n\nResult:\n\n    {\n        \"token\": \"BLOCK\",\n        \"include_fees\": true,\n        \"split_amount_requested\": \"4.0\",\n        \"split_amount_with_fees\": \"4.00040000\",\n        \"split_utxo_count\": 6,\n        \"split_total\": \"24.44852981\",\n        \"txid\": \"7f87cba104b3c19f6e25fbc82b3cde5d73714e01d6a54943d3c8fb07ce315db4\",\n        \"rawtx\": \"\"\n    }\n\n    Key                    | Type | Description\n    -----------------------|------|----------------------------------------------\n    token                  | str  | Asset you are splitting UTXOs for.\n    include_fees           | bool | Whether you requested to include the fees.\n    split_amount_requested | str  | Requested split amount.\n    split_amount_with_fees | str  | Requested split amount with fees included.\n    split_utxo_count       | int  | Amount of resulting split UTXOs.\n    split_total            | str  | Total amount of in the address prior to\n                           |      | splitting.\n    txid                   | str  | Hex string of the splitting transaction.\n    rawtx                  | str  | Hex string of the raw splitting transaction.\n                \nExamples:\n> blocknet-cli dxSplitAddress BLOCK 10.5 BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxSplitAddress\", \"params\": [\"BLOCK\", \"10.5\", \"BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR\"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/\n> blocknet-cli dxSplitAddress BLOCK 10.5 BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR true false true\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxSplitAddress\", \"params\": [\"BLOCK\", \"10.5\", \"BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR\", true, false, true] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpDxSplitInputs              = "dxSplitInputs \"token\" \"split_amount\" \"address\" include_fees show_rawtx submit [{\"txid\":\"hex\",\"vout\":n},...]\n\nSplits specified UTXOs into the given size and address. Left over amounts end up in change. UTXOs being used in existing orders will not be included by the splitter (view with dxGetUtxos). You can only split UTXOs for assets supported by your node (view with dxGetLocalTokens).\n\nArguments:\n1. token                   (string, required) The ticker of the asset you want to split UTXOs for.\n2. split_amount            (string, required) The desired UTXO output size.\n3. address                 (string, required) The address split UTXOs and change will be sent to.\n4. include_fees            (boolean, required) Include the trade P2SH deposit fees in the split UTXO (add deposit fee to `spit_amount` value.\n5. show_rawtx              (boolean, required) Include the raw transaction in the response (can be submitted manually).\n6. submit                  (boolean, required) Submit the raw transaction to the network.\n7. utxos                   (json array, required) List of UTXO inputs.\n     [\n       {                   (json object)\n         \"txid\": \"hex\",    (string, required) The UTXO transaction ID.\n         \"vout\": n,        (numeric, required) The UTXO output index.\n       },\n       ...\n     ]\n\nResult:\n\n    {\n        \"token\": \"BLOCK\",\n        \"include_fees\": true,\n        \"split_amount_requested\": \"4.0\",\n        \"split_amount_with_fees\": \"4.00040000\",\n        \"split_utxo_count\": 6,\n        \"split_total\": \"24.44852981\",\n        \"txid\": \"7f87cba104b2c19f6e25fbc82b3cde5d73714e01d6a54943d3c8fb07ce315db4\",\n        \"rawtx\": \"\"\n    }\n\n    Key                    | Type | Description\n    -----------------------|------|----------------------------------------------\n    token                  | str  | The asset you are splitting UTXOs for.\n    include_fees           | bool | Whether you requested to include the fees.\n    split_amount_requested | str  | The requested split amount.\n    split_amount_with_fees | str  | The requested split amount with fee included.\n    split_utxo_count       | int  | The amount of resulting split UTXOs.\n    split_total            | str  | The total amount of in the address prior to\n                           |      | splitting.\n    txid                   | str  | The hex string of the splitting transaction.\n    rawtx                  | str  | The hex string of the raw splitting\n                           |      | transaction.\n                \nExamples:\n> blocknet-cli dxSplitInputs BLOCK 10.5 BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR true false true [{\"txid\":\"ed7d16abd5c0bf42dec36335d0f63938f1d9c10e7202bc780b888a51d291d3dc\",\"vout\":0},{\"txid\":\"ed7d16abd5c0bf42dec36335d0f63938f1d9c10e7202bc780b888a51d291d3dc\",\"vout\":1}]\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxSplitInputs\", \"params\": [\"BLOCK\", \"10.5\", \"BWQrvmuHB4C68KH5V7fcn9bFtWN8y5hBmR\", true, false, true, [{\"txid\":\"ed7d16abd5c0bf42dec36335d0f63938f1d9c10e7202bc780b888a51d291d3dc\",\"vout\":0},{\"txid\":\"ed7d16abd5c0bf42dec36335d0f63938f1d9c10e7202bc780b888a51d291d3dc\",\"vout\":1}]] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpDxGetUtxos                 = `dxGetUtxos "token" ( include_used )

Returns all compatible and unlocked UTXOs for the specified asset. Currently only P2PKH UTXOs are supported (Segwit UTXOs not supported). You can only view UTXOs for assets supported by your node (view with dxGetLocalTokens).

Arguments:
1. token           (string, required) The ticker of the asset you want to view UTXOs for.
2. include_used    (boolean, optional, default=false) Include UTXOs used in existing orders.

Result:

    [
        {
            "txid": "c019edf2a71efcfc9b1ec50cd0d9db54c55b74acd0bcc81cefd6ffbba359a210",
            "vout": 2,
            "amount": "3.26211780",
            "address": "BrPHj12ZSm7roD2gvrjRG2gD4TzeP1YDXG",
            "scriptPubKey": "7b1ef56a92cec50cd0d147876a914ffd6fcbb4c5724a4057de",
            "confirmations": 11904,
            "orderid": ""
        },
        {
            "txid": "a91c224c0725745cd0bcc81cefd6ffbba3f6cc36956cd566c50cd0d9db5c55b7",
            "vout": 0,
            "amount": "2.44485198",
            "address": "BJYS5dd4Mx5bFxfYDX136SLrv5kGCZaUtF",
            "scriptPubKey": "7e36ab914fc645b2b9fd5ce704f54bc34a59a56c9671eb355b",
            "confirmations": 20690,
            "orderid": "e1b0f4bf05e6c47506abf5d717c95baa1b6de79dd1758673a8cdd171ddad6578"
        }
    ]

    Key             | Type | Description
    ----------------|------|-----------------------------------------------------
    txid            | str  | Transaction ID of the UTXO.
    vout            | int  | Vout index of the UTXO.
    amount          | str  | UTXO amount.
    address         | str  | UTXO address.
    scriptPubKey    | str  | UTXO address script pubkey.
    confirmations   | int  | UTXO blockchain confirmation count.
    orderid         | str  | The order ID if the UTXO is currently being used in
                    |      | an order.
                
Examples:
> blocknet-cli dxGetUtxos BLOCK
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetUtxos", "params": ["BLOCK"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxGetUtxos BTC
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetUtxos", "params": ["BTC"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxGetUtxos BLOCK true
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetUtxos", "params": ["BLOCK", true] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetTradingData = `dxGetTradingData ( blocks errors )

Returns an object of XBridge trading records. This information is pulled from on-chain history so pulling a large amount of blocks will result in longer response times.

Arguments:
1. blocks    (numeric, optional, default=43200) The number of blocks to return trade records for (60s block time).
2. errors    (boolean, optional, default=false) Shows an error if an error is detected.

Result:

    [
      {
        "timestamp": 1559970139,
        "fee_txid": "4b409e5c5fb1986930cf7c19afec2c89ac2ad4fddc13c1d5479b66ddf4a8fefb",
        "nodepubkey": "Bqtms8j1zrE65kcpsEorE5JDzDaHidMtLG",
        "id": "9eb57bac331eab34f3daefd8364cdb2bb05259c407d805d0bd0c",
        "taker": "BLOCK",
        "taker_size": 0.001111,
        "maker": "SYS",
        "maker_size": 0.001000
      },
      {
        "timestamp": 1559970139,
        "fee_txid": "3de7479e8a88ebed986d3b7e7e135291d3fd10e4e6d4c6238663db42c5019286",
        "nodepubkey": "Bqtms8j1zrE65kcpsEorE5JDzDaHidMtLG",
        "id": "fd0fed3ee9fe557d5735768c9bdcd4ab2908165353e0f0cef0d5",
        "taker": "BLOCK",
        "taker_size": 0.001577,
        "maker": "SYS",
        "maker_size": 0.001420
      }
    ]

    Key         | Type | Description
    ------------|------|---------------------------------------------------------
    timestamp   | int  | Unix epoch timestamp of when the trade took place.
    fee_txid    | str  | The Blocknet trade fee transaction ID.
    nodepubkey  | str  | The pubkey of the service node that received the trade
                |      | fee.
    id          | str  | The order ID.
    taker       | str  | Taker trading asset; the ticker of the asset being sold
                |      | by the taker.
    taker_size  | int  | Taker trading size.
    maker       | str  | Maker trading asset; the ticker of the asset being sold
                |      | by the maker.
    maker_size  | int  | Maker trading size.
                
Examples:
> blocknet-cli dxGetTradingData 
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetTradingData", "params": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxGetTradingData 43200
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetTradingData", "params": [43200] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxGetTradingData 43200 true
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetTradingData", "params": [43200, true] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetOrderBook = `dxGetOrderBook detail "maker" "taker" ( max_orders )

This call is used to retrieve open orders at various detail levels:

Detail 1 - Returns the best bid and ask.
Detail 2 - Returns a list of aggregated orders. This is useful for charting.
Detail 3 - Returns a list of non-aggregated orders. This is useful for bot trading.
Detail 4 - Returns the best bid and ask with the order IDs.

Note:
This call will only return orders for markets with both assets supported by your node (view with dxGetLocalTokens). To view all orders, set ShowAllOrders=true in your xbridge.conf header and reload it with dxLoadXBridgeConf.

Arguments:
1. detail        (numeric, required) The detail level.
2. maker         (string, required) The symbol of the token being sold by the maker (e.g. LTC).
3. taker         (string, required) The symbol of the token being sold by the taker (e.g. BLOCK).
4. max_orders    (numeric, optional, default=50) The maximum total orders to display for bids and asks combined.

Result:


Examples:
> blocknet-cli dxGetOrderBook 3 BLOCK LTC
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetOrderBook", "params": [3, "BLOCK", "LTC"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxGetOrderBook 3 BLOCK LTC 60
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetOrderBook", "params": [3, "BLOCK", "LTC", 60] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetOrderFills = `dxGetOrderFills "maker" "taker" ( combined )

Returns all the recent trades by trade pair that have been filled (i.e. completed). This will only return orders that have been filled in your current session.

Arguments:
1. maker       (string, required) The symbol of the asset sold by the maker (e.g. LTC).
2. taker       (string, required) The symbol of the asset sold by the taker (e.g. BLOCK).
3. combined    (boolean, optional, default=true) If true, combines the results to return orders with the maker and taker as specified as well as orders of the inverse market. If false, only returns filled orders with the maker and taker assets as specified.

Result:

    [
        {
            "id": "a1f40d53f75357eb914554359b207b7b745cf096dbcb028eb77b7b7e4043c6b4",
            "time": "2018-01-16T13:15:05.12345Z",
            "maker": "SYS",
            "maker_size": "101.00000000",
            "taker": "LTC",
            "taker_size": "0.01000000"
        },
        {
            "id": "91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9",
            "time": "2018-01-16T13:15:05.12345Z",
            "maker": "LTC",
            "maker_size": "0.01000000",
            "taker": "SYS",
            "taker_size": "101.00000000"
        }
    ]

    Key             | Type | Description
    ----------------|------|-----------------------------------------------------
    Array           | arr  | Array of orders sorted by date descending.
    id              | str  | The order ID.
    time            | str  | Time the order was filled.
    maker           | str  | Maker trading asset; the ticker of the asset being
                    |      | sold by the maker.
    maker_size      | str  | Maker trading size. String is used to preserve
                    |      | precision.
    taker           | str  | Taker trading asset; the ticker of the asset being
                    |      | sold by the taker.
    taker_size      | str  | Taker trading size. String is used to preserve
                    |      | precision.
                
Examples:
> blocknet-cli dxGetOrderFills BLOCK LTC
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetOrderFills", "params": ["BLOCK", "LTC"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxGetOrderFills BLOCK LTC true
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetOrderFills", "params": ["BLOCK", "LTC", true] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetOrderHistory = `dxGetOrderHistory "maker" "taker" start_time end_time granularity ( order_ids with_inverse limit )

Returns the OHLCV data by trade pair for a specified time range and interval. It can return the order history for any asset since all trade history is stored on-chain.

Arguments:
1. maker           (string, required) The symbol of the asset sold by the maker (e.g. LTC).
2. taker           (string, required) The symbol of the asset sold by the taker (e.g. BLOCK).
3. start_time      (numeric, required) The Unix time in seconds for the start time boundary to search.
4. end_time        (numeric, required) The Unix time in seconds for the end time boundary to search.
5. granularity     (numeric, required) Time interval slice in seconds. The slice options are: 60,300,900,3600,21600,86400
6. order_ids       (boolean, optional, default=false) If true, returns the IDs of all filled orders in each slice. If false, IDs are omitted.
7. with_inverse    (boolean, optional, default=false) If false, returns the order history for the specified market. If true, also returns the orders in the inverse pair too (e.g. if LTC SYS then SYS LTC would be returned as well).
8. limit           (numeric, optional, default=2147483647) The max number of interval slices returned. maximum=2147483647

Result:

    [
        //[ time, low, high, open, close, volume, id(s) ],
        [ "2018-01-16T13:15:05.12345Z", 1.10, 2.0, 1.10, 1.4, 1000, [ "0cc2e8a7222f1416cda996031ca21f67b53431614e89651887bc300499a6f83e" ] ],
        [ "2018-01-16T14:15:05.12345Z", 0, 0, 0, 0, 0, [] ],
        [ "2018-01-16T15:15:05.12345Z", 1.12, 2.2, 1.10, 1.4, 1000, [ "91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9", "0cc2e8a7222f1416cda996031ca21f67b53431614e89651887bc300499a6f83e", "a1f40d53f75357eb914554359b207b7b745cf096dbcb028eb77b7b7e4043c6b4" ] ],
        [ "2018-01-16T16:15:05.12345Z", 1.14, 2.0, 1.10, 1.4, 1000, [ "a1f40d53f75357eb914554359b207b7b745cf096dbcb028eb77b7b7e4043c6b4" ] ],
        [ "2018-01-16T17:15:05.12345Z", 1.15, 2.0, 1.10, 1.4, 1000, [ "6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a" ] ]
    ]

    Key           | Type  | Description
    --------------|-------|------------------------------------------------------
    time          | str   | ISO 8601 datetime, with microseconds, of the time at
                  |       | the beginning of the time slice.
    low           | float | Exchange rate lower bound within the time slice.
    high          | float | Exchange rate upper bound within the time slice.
    open          | float | Exchange rate of first filled order at the beginning
                  |       | of the time slice.
    close         | float | Exchange rate of last filled order at the end of the
                  |       | time slice.
    volume        | int   | Total volume of the taker asset within the time
                  |       | slice.
    order_ids     | arr   | Array of GUIDs of all filled orders within the time
                  |       | slice.
                
Examples:
> blocknet-cli dxGetOrderHistory SYS LTC 1540660180 1540660420 60
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetOrderHistory", "params": ["SYS", "LTC", 1540660180, 1540660420, 60] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxGetOrderHistory SYS LTC 1540660180 1540660420 60 true false 18000
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetOrderHistory", "params": ["SYS", "LTC", 1540660180, 1540660420, 60, true, false, 18000] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetTokenBalances = `dxGetTokenBalances

Returns a list of available balances for all connected wallets on your node (view with dxGetLocalTokens).

Note:
These balances do not include Segwit UTXOs or those being used in open or in process orders. XBridge works best with pre-sliced UTXOs so that your entire wallet balance is capable of multiple simultaneous trades. Use dxSplitInputs or dxSplitAddress to generate trading inputs.

Result:

    {
        "BLOCK": "250.83492174",
        "LTC": "0.568942",
        "MONA": "3.452",
        "SYS": "1050.128493"
    }

    Key          | Type | Description
    -------------|------|--------------------------------------------------------
    Object       | obj  | Key-value object of the assets and respective balances.
    -- key       | str  | The asset symbol.
    -- value     | str  | The available wallet balance amount.
                
Examples:
> blocknet-cli dxGetTokenBalances 
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetTokenBalances", "params": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetLocalTokens = `dxGetLocalTokens

Returns a list of assets supported by your node. You can only trade on markets with assets returned in both dxGetNetworkTokens and dxGetLocalTokens.

Result:

    [
        "BLOCK",
        "LTC",
        "MONA",
        "SYS"
    ]

    Key                    | Type | Description
    -----------------------|------|----------------------------------------------
    Array                  | arr  | An array of all the assets supported by the
                           |      | local client.
                
Examples:
> blocknet-cli dxGetLocalTokens 
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetLocalTokens", "params": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetNetworkTokens = `dxGetNetworkTokens

Returns a list of all the assets currently supported by the network. You can only trade on markets with assets returned in both dxGetNetworkTokens and dxGetLocalTokens.

Result:

    [
        "BLOCK",
        "BTC",
        "DGB",
        "LTC",
        "MONA",
        "PIVX",
        "SYS"
    ]

    Key                    | Type | Description
    -----------------------|------|----------------------------------------------
    Array                  | arr  | An array of all the assets supported by the
                           |      | network.
                
Examples:
> blocknet-cli dxGetNetworkTokens 
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetNetworkTokens", "params": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetNewTokenAddress = `dxGetNewTokenAddress "ticker"

Returns a new address for the specified asset.

Arguments:
1. ticker    (string, required) The ticker symbol of the asset you want to generate an address for (e.g. LTC).

Result:

    [
        "SVTbaYZ8oApVn3uNyimst3GKyvvfzXQgdK"
    ]

    Key                    | Type | Description
    -----------------------|------|----------------------------------------------
    Array                  | arr  | An array containing the newly generated
                           |      | address for the given asset.
                
Examples:
> blocknet-cli dxGetNewTokenAddress BTC
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetNewTokenAddress", "params": ["BTC"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxGetLockedUtxos = `dxGetLockedUtxos ( "id" )

Returns a list of locked UTXOs used in orders. You can only use this call if you have a Service Node setup.

Arguments:
1. id    (string) The order ID. If omitted, a list of UTXOs used in all orders will be returned.

Result:

    [
        {
            "id" : "91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9",
            "LTC" : [
                6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a,
                6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a,
                6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a
            ]
        }
    ]

    Key             | Type | Description
    ----------------|------|-----------------------------------------------------
    id              | str  | The order ID.
    Object          | obj  | Key-value object of the asset and UTXOs for the
                    |      | forementioned order.
    -- key          | str  | The asset symbol.
    -- value        | arr  | The UTXOs locked for the given order ID.
                
Examples:
> blocknet-cli dxGetLockedUtxos 
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetLockedUtxos", "params": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxGetLockedUtxos 524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxGetLockedUtxos", "params": ["524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxCancelOrder = `dxCancelOrder "id"

This call is used to cancel one of your own orders. This automatically rolls back the order if a trade is in process.

Arguments:
1. id    (string, required) The ID of the order to cancel.

Result:

    {
        "id": "91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9",
        "maker": "SYS",
        "maker_size": "0.100",
        "maker_address": "SVTbaYZ8oApVn3uNyimst3GKyvvfzXQgdK",
        "taker": "LTC",
        "taker_size": "0.01",
        "taker_address": "LVvFhzRoMRGTtGihHp7jVew3YoZRX8y35Z",
        "updated_at": "1970-01-01T00:00:00.00000Z",
        "created_at": "2018-01-15T18:15:30.12345Z",
        "status": "canceled"
    }

    Key             | Type | Description
    ----------------|------|-----------------------------------------------------
    id              | str  | The order ID.
    maker           | str  | Sending asset of party cancelling the order.
    maker_size      | str  | Sending trading size. String is used to preserve
                    |      | precision.
    maker_address   | str  | Address for sending the outgoing asset.
    taker           | str  | Receiving asset of party cancelling the order.
    taker_size      | str  | Receiving trading size. String is used to preserve
                    |      | precision.
    taker_address   | str  | Address for receiving the incoming asset.
    updated_at      | str  | ISO 8601 datetime, with microseconds, of the last
                    |      | time the order was updated.
    created_at      | str  | ISO 8601 datetime, with microseconds, of when the
                    |      | order was created.
    status          | str  | The order status (canceled).
                
Examples:
> blocknet-cli dxCancelOrder 524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxCancelOrder", "params": ["524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxFlushCancelledOrders = `dxFlushCancelledOrders ( ageMillis )

This call is used to remove your cancelled orders that are older than the specified amount of time.

Arguments:
1. ageMillis    (numeric, optional, default=0) Remove cancelled orders older than this amount of milliseconds.

Result:

    {
        "ageMillis": 0,
        "now": "20191126T024005.352285",
        "durationMicrosec": 0,
        "flushedOrders": [
            {
                "id": "582a02ada05c8a4bb39b34de0eb54767bcb95a7792e5865d3a0babece4715f47",
                "txtime": "20191126T023945.855058",
                "use_count": 1
            },
            {
                "id": "a508cd8d110bdc0b1fd819a89d94cdbf702e3aa40edbe654af5d556ff3c43a0a",
                "txtime": "20191126T023956.270409",
                "use_count": 1
            }
        ]
    }

    Key               | Type | Description
    ------------------|------|---------------------------------------------------
    ageMillis         | int  | Millisecond value specified when making the call.
    now               | str  | ISO 8601 datetime, with microseconds, of when the
                      |      | call was executed.
    durationMicrosec* | int  | The amount of time in milliseconds it took to
                      |      | process the call.
    flushedOrders     | arr  | Array of cancelled orders that were removed.
    id                | str  | The order ID.
    txtime            | str  | ISO 8601 datetime, with microseconds, of when the
                      |      | order was created.
    use_count*        | int  | This value is strictly for debugging purposes.
                
Examples:
> blocknet-cli dxFlushCancelledOrders 
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxFlushCancelledOrders", "params": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/
> blocknet-cli dxFlushCancelledOrders 600000
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "dxFlushCancelledOrders", "params": [600000] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxLoadXBridgeConf = "dxLoadXBridgeConf\n\nHot loads the xbridge.conf file. Note, this may disrupt trades in progress.\n\nResult:\n\n    true\n\n    Type | Description\n    -----|----------------------------------------------\n    bool | `true`: Successfully reloaded file.\n                \nExamples:\n> blocknet-cli dxLoadXBridgeConf \n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxLoadXBridgeConf\", \"params\": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpGetnetworkinfo    = `getnetworkinfo
Returns an object containing various state info regarding P2P networking.

Result:
{
  "version": xxxxx,                      (numeric) the server version
  "subversion": "/Satoshi:x.x.x/",     (string) the server subversion string
  "protocolversion": xxxxx,              (numeric) the protocol version
  "xbridgeprotocolversion": xxxxx,       (numeric) the XBridge protocol version
  "xrouterprotocolversion": xxxxx,       (numeric) the XRouter protocol version
  "localservices": "xxxxxxxxxxxxxxxx", (string) the services we offer to the network
  "localrelay": true|false,              (bool) true if transaction relay is requested from peers
  "timeoffset": xxxxx,                   (numeric) the time offset
  "connections": xxxxx,                  (numeric) the number of connections
  "networkactive": true|false,           (bool) whether p2p networking is enabled
  "networks": [                          (array) information per network
  {
    "name": "xxx",                     (string) network (ipv4, ipv6 or onion)
    "limited": true|false,               (boolean) is the network limited using -onlynet?
    "reachable": true|false,             (boolean) is the network reachable?
    "proxy": "host:port"               (string) the proxy that is used for this network, or empty if none
    "proxy_randomize_credentials": true|false,  (string) Whether randomized credentials are used
  }
  ,...
  ],
  "relayfee": x.xxxxxxxx,                (numeric) minimum relay fee for transactions in BLOCK/kB
  "incrementalfee": x.xxxxxxxx,          (numeric) minimum fee increment for mempool limiting or BIP 125 replacement in BLOCK/kB
  "localaddresses": [                    (array) list of local addresses
  {
    "address": "xxxx",                 (string) network address
    "port": xxx,                         (numeric) network port
    "score": xxx                         (numeric) relative score
  }
  ,...
  ]
  "warnings": "..."                    (string) any network and blockchain warnings
}

Examples:
> blocknet-cli getnetworkinfo 
> curl --user myusername --data-binary '{"jsonrpc": "1.0", "id":"curltest", "method": "getnetworkinfo", "params": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/`
	helpDxHelp        = "help ( \"command\" )\n\nList all commands, or get help for a specified command.\n\nArguments:\n1. command    (string, optional, default=all commands) The command to get help on\n\nResult:\n\"text\"     (string) The help text\n"
	helpDxGetOrders   = "dxGetOrders\n\nReturns a list of all orders of every market pair. \nIt will only return orders for assets returned in dxGetLocalTokens.\n\nResult:\n\n    [\n        {\n            \"id\": \"91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9\",\n            \"maker\": \"SYS\",\n            \"maker_size\": \"100.000000\",\n            \"taker\": \"LTC\",\n            \"taker_size\": \"10.500000\",\n            \"updated_at\": \"2018-01-15T18:25:05.12345Z\",\n            \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n            \"order_type\": \"partial\",\n            \"partial_minimum\": \"10.000000\",\n            \"partial_orig_maker_size\": \"100.000000\",\n            \"partial_orig_taker_size\": \"10.500000\",\n            \"partial_repost\": false,\n            \"partial_parent_id\": \"\",\n            \"status\": \"open\"\n        },\n        {\n            \"id\": \"a1f40d53f75357eb914554359b207b7b745cf096dbcb028eb77b7b7e4043c6b4\",\n            \"maker\": \"SYS\",\n            \"maker_size\": \"0.100000\",\n            \"taker\": \"LTC\",\n            \"taker_size\": \"0.010000\",\n            \"updated_at\": \"2018-01-15T18:25:05.12345Z\",\n            \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n            \"order_type\": \"exact\",\n            \"partial_minimum\": \"0.000000\",\n            \"partial_orig_maker_size\": \"0.000000\",\n            \"partial_orig_taker_size\": \"0.000000\",\n            \"partial_repost\": false,\n            \"partial_parent_id\": \"\",\n            \"status\": \"open\"\n        }\n    ]\n\n    Key                     | Type | Description\n    ------------------------|------|---------------------------------------------\n    Array                   | arr  | An array of all orders with each order\n                            |      | having the following parameters.\n    id                      | str  | The order ID.\n    maker                   | str  | Maker trading asset; the ticker of the asset\n                            |      | being sold by the maker.\n    maker_size              | str  | Maker trading size. String is used to\n                            |      | preserve precision.\n    maker_address           | str  | Address for sending the outgoing asset.\n    taker                   | str  | Taker trading asset; the ticker of the asset\n                            |      | being sold by the taker.\n    taker_size              | str  | Taker trading size. String is used to\n                            |      | preserve precision.\n    taker_address           | str  | Address for receiving the incoming asset.\n    updated_at              | str  | ISO 8601 datetime, with microseconds, of the\n                            |      | last time the order was updated.\n    created_at              | str  | ISO 8601 datetime, with microseconds, of\n                            |      | when the order was created.\n    order_type              | str  | The order type.\n    partial_minimum*        | str  | The minimum amount that can be taken.\n    partial_orig_maker_size*| str  | The partial order original maker_size.\n    partial_orig_taker_size*| str  | The partial order original taker_size.\n    partial_repost          | str  | Whether the order will be reposted or not.\n                            |      | This applies to `partial` order types and\n                            |      | will show `false` for `exact` order types.\n    partial_parent_id       | str  | The previous order id of a reposted partial\n                            |      | order. This will return an empty string if\n                            |      | there is no parent order.\n    status                  | str  | The order status.\n\n    * This only applies to `partial` order types and will show `0` on `exact`\n      order types.\n                \nExamples:\n> blocknet-cli dxGetOrders \n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxGetOrders\", \"params\": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpDxGetOrder    = "dxGetOrder \"id\"\n\nReturns order info by order ID.\n\nArguments:\n1. id    (string, required) The order ID.\n\nResult:\n\n    {\n        \"id\": \"6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a\",\n        \"maker\": \"SYS\",\n        \"maker_size\": \"0.100\",\n        \"taker\": \"LTC\",\n        \"taker_size\": \"0.01\",\n        \"updated_at\": \"1970-01-01T00:00:00.00000Z\",\n        \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n        \"order_type\": \"exact\",\n        \"partial_minimum\": \"0.000000\",\n        \"partial_orig_maker_size\": \"0.000000\",\n        \"partial_orig_taker_size\": \"0.000000\",\n        \"partial_repost\": false,\n        \"partial_parent_id\": \"\",\n        \"status\": \"open\"\n    }\n\n    Key                     | Type | Description\n    ------------------------|------|---------------------------------------------\n    Array                   | arr  | An array of all orders with each order\n                            |      | having the following parameters.\n    id                      | str  | The order ID.\n    maker                   | str  | Maker trading asset; the ticker of the asset\n                            |      | being sold by the maker.\n    maker_size              | str  | Maker trading size. String is used to\n                            |      | preserve precision.\n    maker_address           | str  | Address for sending the outgoing asset.\n    taker                   | str  | Taker trading asset; the ticker of the asset\n                            |      | being sold by the taker.\n    taker_size              | str  | Taker trading size. String is used to\n                            |      | preserve precision.\n    taker_address           | str  | Address for receiving the incoming asset.\n    updated_at              | str  | ISO 8601 datetime, with microseconds, of the\n                            |      | last time the order was updated.\n    created_at              | str  | ISO 8601 datetime, with microseconds, of\n                            |      | when the order was created.\n    order_type              | str  | The order type.\n    partial_minimum*        | str  | The minimum amount that can be taken.\n    partial_orig_maker_size*| str  | The partial order original maker_size.\n    partial_orig_taker_size*| str  | The partial order original taker_size.\n    partial_repost          | str  | Whether the order will be reposted or not.\n                            |      | This applies to `partial` order types and\n                            |      | will show `false` for `exact` order types.\n    partial_parent_id       | str  | The previous order id of a reposted partial\n                            |      | order. This will return an empty string if\n                            |      | there is no parent order.\n    status                  | str  | The order status.\n\n    * This only applies to `partial` order types and will show `0` on `exact`\n      order types.\n                \nExamples:\n> blocknet-cli dxGetOrder 524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432\n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxGetOrder\", \"params\": [\"524137449d9a35fa707ee395abab32bedae91aa2aefb6e3611fcd8574863e432\"] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
	helpDxGetMyOrders = "dxGetMyOrders\n\nReturns a list of all of your orders (of all states). It will only return orders from your current session.\n\nResult:\n\n    [\n        {\n            \"id\": \"91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9\",\n            \"maker\": \"SYS\",\n            \"maker_size\": \"100.000000\",\n            \"maker_address\": \"SVTbaYZ8olpVn3uNyImst3GKyrvfzXQgdK\",\n            \"taker\": \"LTC\",\n            \"taker_size\": \"10.500000\",\n            \"taker_address\": \"LVvFhZroMRGTtg1hHp7jVew3YoZRX8y35Z\",\n            \"updated_at\": \"2018-01-15T18:25:05.12345Z\",\n            \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n            \"order_type\": \"partial\",\n            \"partial_minimum\": \"10.000000\",\n            \"partial_orig_maker_size\": \"100.000000\",\n            \"partial_orig_taker_size\": \"10.500000\",\n            \"partial_repost\": true,\n            \"partial_parent_id\": \"\",\n            \"status\": \"open\"\n        },\n        {\n            \"id\": \"6be548bc46a3dcc69b6d56529948f7e679dd96657f85f5870a017e005caa050a\",\n            \"maker\": \"SYS\",\n            \"maker_size\": \"4.000000\",\n            \"maker_address\": \"SVTbaYZ8olpVn3uNyImst3GKyrvfzXQgdK\",\n            \"taker\": \"LTC\",\n            \"taker_size\": \"0.400000\",\n            \"taker_address\": \"LVvFhZroMRGTtg1hHp7jVew3YoZRX8y35Z\",\n            \"updated_at\": \"2018-01-15T18:25:05.12345Z\",\n            \"created_at\": \"2018-01-15T18:15:30.12345Z\",\n            \"order_type\": \"partial\",\n            \"partial_minimum\": \"0.400000\",\n            \"partial_orig_maker_size\": \"4.000000\",\n            \"partial_orig_taker_size\": \"0.400000\",\n            \"partial_repost\": true,\n            \"partial_parent_id\": \"91d0ea83edc79b9a2041c51d08037cff87c181efb311a095dfdd4edbcc7993a9\",\n            \"status\": \"open\"\n        }\n    ]\n\n    Key                     | Type | Description\n    ------------------------|------|---------------------------------------------\n    Array                   | arr  | An array of all orders with each order\n                            |      | having the following parameters.\n    id                      | str  | The order ID.\n    maker                   | str  | Maker trading asset; the ticker of the asset\n                            |      | being sold by the maker.\n    maker_size              | str  | Maker trading size. String is used to\n                            |      | preserve precision.\n    maker_address           | str  | Address for sending the outgoing asset.\n    taker                   | str  | Taker trading asset; the ticker of the asset\n                            |      | being sold by the taker.\n    taker_size              | str  | Taker trading size. String is used to\n                            |      | preserve precision.\n    taker_address           | str  | Address for receiving the incoming asset.\n    updated_at              | str  | ISO 8601 datetime, with microseconds, of the\n                            |      | last time the order was updated.\n    created_at              | str  | ISO 8601 datetime, with microseconds, of\n                            |      | when the order was created.\n    order_type              | str  | The order type.\n    partial_minimum*        | str  | The minimum amount that can be taken.\n    partial_orig_maker_size*| str  | The partial order original maker_size.\n    partial_orig_taker_size*| str  | The partial order original taker_size.\n    partial_repost          | str  | Whether the order will be reposted or not.\n                            |      | This applies to `partial` order types and\n                            |      | will show `false` for `exact` order types.\n    partial_parent_id       | str  | The previous order id of a reposted partial\n                            |      | order. This will return an empty string if\n                            |      | there is no parent order.\n    status                  | str  | The order status.\n\n    * This only applies to `partial` order types and will show `0` on `exact`\n      order types.\n                \nExamples:\n> blocknet-cli dxGetMyOrders \n> curl --user myusername --data-binary '{\"jsonrpc\": \"1.0\", \"id\":\"curltest\", \"method\": \"dxGetMyOrders\", \"params\": [] }' -H 'content-type: text/plain;' http://127.0.0.1:41414/"
)
