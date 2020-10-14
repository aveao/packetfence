package remoteclients

import "net"

const (
	AUTH_TIMESTAMP_START = 0
	AUTH_TIMESTAMP_END   = 8
	AUTH_RAND_START      = AUTH_TIMESTAMP_END
	AUTH_RAND_END        = 40
	AUTH_PUB_START       = AUTH_RAND_END
	AUTH_PUB_END         = 72
)

type Peer struct {
	WireguardIP      net.IP   `json:"wireguard_ip"`
	WireguardNetmask int      `json:"wireguard_netmask"`
	PublicKey        string   `json:"public_key"`
	AllowedPeers     []string `json:"allowed_peers,omitempty"`
}
