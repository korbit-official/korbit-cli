// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package rawapi

// Shared value types used across the endpoint domains. Money and quantity
// values are decimal strings and are NEVER modeled as float types.

// Symbol is a trading pair, e.g. "btc_krw".
type Symbol string

// Currency is an asset symbol, e.g. "btc".
type Currency string

// Side is an order side.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// OrderType is an order type.
type OrderType string

const (
	OrderTypeLimit  OrderType = "limit"
	OrderTypeMarket OrderType = "market"
	OrderTypeBest   OrderType = "best"
)

// TimeInForce is an order time-in-force.
type TimeInForce string

const (
	TimeInForceGTC TimeInForce = "gtc"
	TimeInForceIOC TimeInForce = "ioc"
	TimeInForceFOK TimeInForce = "fok"
	TimeInForcePO  TimeInForce = "po"
)
