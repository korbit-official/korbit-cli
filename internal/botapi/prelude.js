// Prelude for the monitor's script runtime. Everything here is global and
// persists across events. Names starting with __ are internal plumbing.

// __rows normalizes an event payload to its array of records, whatever shape
// the document took: a bare array (public trade frames, REST backfill lists),
// the public `data` wrapper (ticker/orderbook frames carry one object; trade
// frames an array), or the private wrappers (order.orders / trade.trades /
// asset.assets). Anything unrecognized becomes a one-element array so
// `rows.some(...)` is always safe to write.
function __rows(payload) {
  if (payload === null || payload === undefined) return [];
  if (Array.isArray(payload)) return payload;
  if (typeof payload !== 'object') return [payload];
  var d = payload.data;
  if (Array.isArray(d)) return d;
  if (d && typeof d === 'object') return [d];
  if (payload.order && Array.isArray(payload.order.orders)) return payload.order.orders;
  if (payload.trade && Array.isArray(payload.trade.trades)) return payload.trade.trades;
  if (payload.asset && Array.isArray(payload.asset.assets)) return payload.asset.assets;
  return [payload];
}

// __settle reports a promise's settlement to a Go callback: done(true, value)
// on fulfillment, done(false, reason) on rejection. Promise.resolve() makes it
// safe for handlers that return a plain value instead of a promise.
function __settle(p, done) {
  Promise.resolve(p).then(
    function (v) { done(true, v); },
    function (e) { done(false, e); }
  );
}
