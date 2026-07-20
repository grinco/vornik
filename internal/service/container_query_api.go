package service

import (
	"time"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/apigateway/gateway"
	"vornik.io/vornik/internal/config"
)

// gatewayClientTimeout bounds every daemon→gateway request. The gateway is
// local (loopback), so this is generous headroom rather than a tight SLA.
const gatewayClientTimeout = 30 * time.Second

// newGatewayClient builds the query_api gateway client from config. Returns
// (nil,nil) when disabled or unauthenticated (fail-closed — the tool then
// reports "not configured", design §5.1). Maps config.Providers → registry.
func newGatewayClient(cfg config.GatewayConfig) (apigateway.Client, error) {
	if !cfg.Enabled || cfg.Address == "" || cfg.Token == "" {
		return nil, nil
	}
	reg := make(apigateway.Registry, len(cfg.Providers))
	for name, p := range cfg.Providers {
		reg[name] = apigateway.Provider{
			BasePath:       p.BasePath,
			AllowedMethods: p.AllowedMethods,
			WritesEnabled:  p.WritesEnabled,
			Description:    p.Description,
			Examples:       p.Examples,
		}
	}
	return gateway.New(cfg.Address, cfg.Token, reg, gatewayClientTimeout)
}
