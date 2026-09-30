package main

import (
	"fmt"
	"net"
	"sort"
)

// LocalSubnet is one address space of this computer that may hold printers.
type LocalSubnet struct {
	Prefix string `json:"prefix"`
	Iface  string `json:"iface"`
	IP     string `json:"ip"`
	Mask   int    `json:"mask"`
}

// hostPrefixes returns just the prefixes of this machine's own subnets.
func hostPrefixes() []string {
	var out []string
	for _, s := range hostSubnets() {
		out = append(out, s.Prefix)
	}
	return out
}

// hostSubnets returns the /24 prefix of every IPv4 address of this machine —
// то, что можно предложить владельцу на первом запуске вместо угадывания.
func hostSubnets() []LocalSubnet {
	var out []LocalSubnet
	seen := map[string]bool{}

	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			prefix := fmt.Sprintf("%d.%d.%d.", ip4[0], ip4[1], ip4[2])
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			ones, _ := ipnet.Mask.Size()
			out = append(out, LocalSubnet{
				Prefix: prefix,
				Iface:  iface.Name,
				IP:     ip4.String(),
				Mask:   ones,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Prefix < out[j].Prefix })
	return out
}
