package planner

import (
	"reflect"
	"testing"

	"github.com/rancher/rancher/pkg/data/management"
	"github.com/rancher/wrangler/v3/pkg/data/convert"
)

func TestUpdateConfigWithAddresses(t *testing.T) {
	tests := []struct {
		name                    string
		initialConfig           map[string]interface{}
		info                    *machineNetworkInfo
		primaryStack            string
		stack                   string
		expectedNodeIPs         []string
		expectedNodeExternalIPs []string
	}{
		{
			name: "AWS dual-stack node",
			initialConfig: map[string]interface{}{
				"node-ip":             []string{},
				"node-external-ip":    []string{},
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5"},
				ExternalAddresses: []string{"1.2.3.4"},
				IPv6Address:       "2001:db8::1",
				DriverName:        management.Amazonec2driver,
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.0.0.5", "2001:db8::1"},
			expectedNodeExternalIPs: []string{"1.2.3.4"},
		},
		{
			name: "AWS IPv4-only node",
			initialConfig: map[string]interface{}{
				"node-ip":             []string{},
				"node-external-ip":    []string{},
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5"},
				ExternalAddresses: []string{"1.2.3.4"},
				DriverName:        management.Amazonec2driver,
			},
			primaryStack:            "ipv4",
			stack:                   "ipv4",
			expectedNodeIPs:         []string{"10.0.0.5"},
			expectedNodeExternalIPs: []string{"1.2.3.4"},
		},
		{
			name: "AWS IPv6-only node",
			initialConfig: map[string]interface{}{
				"node-ip":             []string{},
				"node-external-ip":    []string{},
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				IPv6Address: "2001:db8::1",
				DriverName:  management.Amazonec2driver,
			},
			primaryStack:            "ipv6",
			stack:                   "ipv6",
			expectedNodeIPs:         []string{"2001:db8::1"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "DigitalOcean IPv4-only with internal IP",
			initialConfig: map[string]interface{}{
				"node-ip":             []string{},
				"node-external-ip":    []string{},
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.1.2.3"},
				ExternalAddresses: []string{"203.0.113.1"},
				DriverName:        management.DigitalOceandriver,
			},
			primaryStack:            "ipv4",
			stack:                   "ipv4",
			expectedNodeIPs:         []string{"10.1.2.3"},
			expectedNodeExternalIPs: []string{"203.0.113.1"},
		},
		{
			name: "DigitalOcean driver IPv4-only with no internal IP",
			initialConfig: map[string]interface{}{
				"node-ip":             []string{},
				"node-external-ip":    []string{},
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{},
				ExternalAddresses: []string{"203.0.113.1"},
				DriverName:        management.DigitalOceandriver,
			},
			primaryStack:            "ipv4",
			stack:                   "ipv4",
			expectedNodeIPs:         []string{"203.0.113.1"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "DigitalOcean driver dual-stack with internal IP",
			initialConfig: map[string]interface{}{
				"node-ip":             []string{},
				"node-external-ip":    []string{},
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.1.2.3"},
				ExternalAddresses: []string{"203.0.113.1"},
				IPv6Address:       "2001:db8::1",
				DriverName:        management.DigitalOceandriver,
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.1.2.3", "2001:db8::1"},
			expectedNodeExternalIPs: []string{"203.0.113.1"},
		},
		{
			name: "DigitalOcean driver IPv6-only with internal IP",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.1.2.3"},
				ExternalAddresses: []string{"203.0.113.1"},
				IPv6Address:       "2001:db8:2::1",
				DriverName:        management.DigitalOceandriver,
			},
			primaryStack:            "ipv6",
			stack:                   "ipv6",
			expectedNodeIPs:         []string{"2001:db8:2::1"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "DigitalOcean driver IPv6-only with no internal IP",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{},
				ExternalAddresses: []string{"203.0.113.1"},
				IPv6Address:       "2001:db8:2::1",
				DriverName:        management.DigitalOceandriver,
			},
			primaryStack:            "ipv6",
			stack:                   "ipv6",
			expectedNodeIPs:         []string{"2001:db8:2::1"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "Pod driver skips node-ip assignment",
			initialConfig: map[string]interface{}{
				"node-ip":             []string{},
				"node-external-ip":    []string{},
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.10.10.5"},
				ExternalAddresses: []string{"172.16.1.5"},
				DriverName:        management.PodDriver,
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "Cloud provider set disables external IP assignment",
			initialConfig: map[string]interface{}{
				"node-ip":             []string{},
				"node-external-ip":    []string{},
				"cloud-provider-name": "aws",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.7"},
				ExternalAddresses: []string{"203.0.113.5"},
				IPv6Address:       "2001:db8::7",
				DriverName:        "amazonec2",
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.0.0.7", "2001:db8::7"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "Multiple internal and external IPs",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5", "10.0.0.6"},
				ExternalAddresses: []string{"1.2.3.4", "1.2.3.5"},
				IPv6Address:       "2001:db8::1",
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.0.0.5", "10.0.0.6", "2001:db8::1"},
			expectedNodeExternalIPs: []string{"1.2.3.4", "1.2.3.5"},
		},
		{
			name: "Multiple internal IPs, one is IPv6",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"2001:db8::2", "10.0.0.6"},
				ExternalAddresses: []string{"1.2.3.4"},
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.0.0.6", "2001:db8::2"},
			expectedNodeExternalIPs: []string{"1.2.3.4"},
		},
		{
			name: "Multiple internal IPs, no IPv4",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"2001:db8::2", "2001:db8::3"},
				ExternalAddresses: []string{"1.2.3.4"},
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"2001:db8::2", "2001:db8::3"},
			expectedNodeExternalIPs: []string{"1.2.3.4"},
		},
		{
			name: "Duplicated internal and external IPs",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"2001:db8::2", "10.0.0.6"},
				ExternalAddresses: []string{"2001:db8::2", "10.0.0.6"},
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.0.0.6", "2001:db8::2"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "Duplicated internal and external and IPv6",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"1.2.3.4", "1.2.3.5", "1.2.3.7"},
				ExternalAddresses: []string{"1.2.3.4", "1.2.3.5", "1.2.3.6"},
				IPv6Address:       "2001:db8::1",
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"1.2.3.4", "1.2.3.5", "1.2.3.7", "2001:db8::1"},
			expectedNodeExternalIPs: []string{"1.2.3.6"},
		},
		{
			name: "IPv4 stack with dual-stack IPs available",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5", "2001:db8::2"},
				ExternalAddresses: []string{"1.2.3.4"},
			},
			primaryStack:            "ipv4",
			stack:                   "ipv4",
			expectedNodeIPs:         []string{"10.0.0.5"},
			expectedNodeExternalIPs: []string{"1.2.3.4"},
		},
		{
			name: "IPv6 stack with dual-stack IPs available",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5", "2001:db8::2"},
				ExternalAddresses: []string{"1.2.3.4"},
			},
			primaryStack:            "ipv6",
			stack:                   "ipv6",
			expectedNodeIPs:         []string{"2001:db8::2"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "Dual-stack IPv4 primary with dual-stack external IPs",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5"},
				ExternalAddresses: []string{"1.2.3.4", "2001:db8::ffff"},
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.0.0.5"},
			expectedNodeExternalIPs: []string{"1.2.3.4", "2001:db8::ffff"},
		},
		{
			name: "Dual-stack IPv6 primary with dual-stack external IPs",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5"},
				ExternalAddresses: []string{"1.2.3.4", "2001:db8::ffff"},
			},
			primaryStack:            "ipv6",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.0.0.5"},
			expectedNodeExternalIPs: []string{"2001:db8::ffff", "1.2.3.4"},
		},
		{
			name: "Dual-stack IPv4 primary with only IPv6 external IP",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5"},
				ExternalAddresses: []string{"2001:db8::ffff"},
			},
			primaryStack:            "ipv4",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"10.0.0.5"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "Dual-stack IPv6 primary with only IPv4 external IP",
			initialConfig: map[string]interface{}{
				"cloud-provider-name": "",
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"2001:db8::2"},
				ExternalAddresses: []string{"1.2.3.4"},
			},
			primaryStack:            "ipv6",
			stack:                   "dual-stack",
			expectedNodeIPs:         []string{"2001:db8::2"},
			expectedNodeExternalIPs: []string{},
		},
		{
			name: "Initial IPs are preserved and not duplicated",
			initialConfig: map[string]interface{}{
				"node-ip": []string{"10.0.0.5"},
			},
			info: &machineNetworkInfo{
				InternalAddresses: []string{"10.0.0.5", "10.0.0.6"},
			},
			primaryStack:            "ipv4",
			stack:                   "ipv4",
			expectedNodeIPs:         []string{"10.0.0.5", "10.0.0.6"},
			expectedNodeExternalIPs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := make(map[string]interface{}, len(tt.initialConfig))
			for k, v := range tt.initialConfig {
				config[k] = v
			}
			updateConfigWithAddresses(config, tt.info, tt.stack, tt.primaryStack)

			gotIPs := convert.ToStringSlice(config["node-ip"])
			if !reflect.DeepEqual(gotIPs, tt.expectedNodeIPs) {
				// Pod driver is a special case where we expect an empty list, but the list might be nil
				if tt.info.DriverName == management.PodDriver && len(gotIPs) == 0 && len(tt.expectedNodeIPs) == 0 {
					// This is fine
				} else {
					t.Errorf("node-ip mismatch:\n  got:  %v\n  want: %v", gotIPs, tt.expectedNodeIPs)
				}
			}

			gotExternal := convert.ToStringSlice(config["node-external-ip"])
			if len(tt.expectedNodeExternalIPs) > 0 {
				if !reflect.DeepEqual(gotExternal, tt.expectedNodeExternalIPs) {
					t.Errorf("node-external-ip mismatch:\n  got  %v\n  want %v", gotExternal, tt.expectedNodeExternalIPs)
				}
			} else {
				if len(gotExternal) > 0 {
					t.Errorf("unexpected node-external-ip: %v", gotExternal)
				}
			}
		})
	}
}

func TestGetStack(t *testing.T) {
	tests := []struct {
		name                 string
		config               map[string]interface{}
		expectedStack        string
		expectedPrimaryStack string
		expectError          bool
	}{
		{
			name:                 "No cluster-cidr",
			config:               map[string]interface{}{},
			expectedStack:        "ipv4",
			expectedPrimaryStack: "ipv4",
			expectError:          false,
		},
		{
			name:                 "Empty cluster-cidr",
			config:               map[string]interface{}{"cluster-cidr": ""},
			expectedStack:        "ipv4",
			expectedPrimaryStack: "ipv4",
			expectError:          false,
		},
		{
			name:                 "Single IPv4 CIDR",
			config:               map[string]interface{}{"cluster-cidr": "10.42.0.0/16"},
			expectedStack:        "ipv4",
			expectedPrimaryStack: "ipv4",
			expectError:          false,
		},
		{
			name:                 "Single IPv6 CIDR",
			config:               map[string]interface{}{"cluster-cidr": "2001:cafe:42:0::/56"},
			expectedStack:        "ipv6",
			expectedPrimaryStack: "ipv6",
			expectError:          false,
		},
		{
			name:                 "Dual-stack CIDR",
			config:               map[string]interface{}{"cluster-cidr": "10.42.0.0/16,2001:cafe:42:0::/56"},
			expectedStack:        "dual-stack",
			expectedPrimaryStack: "ipv4",
			expectError:          false,
		},
		{
			name:                 "Dual-stack CIDR with spaces",
			config:               map[string]interface{}{"cluster-cidr": "  10.42.0.0/16 , 2001:cafe:42:0::/56  "},
			expectedStack:        "dual-stack",
			expectedPrimaryStack: "ipv4",
			expectError:          false,
		},
		{
			name:                 "Dual-stack CIDR IPv6 primary",
			config:               map[string]interface{}{"cluster-cidr": "2001:cafe:42:0::/56,10.42.0.0/16"},
			expectedStack:        "dual-stack",
			expectedPrimaryStack: "ipv6",
			expectError:          false,
		},
		{
			name:        "Invalid CIDR",
			config:      map[string]interface{}{"cluster-cidr": "10.42.0.0/33"},
			expectError: true,
		},
		{
			name:        "Two IPv4 CIDRs",
			config:      map[string]interface{}{"cluster-cidr": "10.42.0.0/16,10.43.0.0/16"},
			expectError: true,
		},
		{
			name:        "Three CIDRs",
			config:      map[string]interface{}{"cluster-cidr": "10.42.0.0/16,2001:cafe:42:0::/56,10.43.0.0/16"},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stack, primaryStack, err := getStack(tt.config)
			if (err != nil) != tt.expectError {
				t.Errorf("getStack() error = %v, expectError %v", err, tt.expectError)
				return
			}
			if stack != tt.expectedStack {
				t.Errorf("getStack() stack = %v, want %v", stack, tt.expectedStack)
			}
			if primaryStack != tt.expectedPrimaryStack {
				t.Errorf("getStack() primaryStack = %v, want %v", primaryStack, tt.expectedPrimaryStack)
			}
		})
	}
}

func TestFilterIPsByFamily(t *testing.T) {
	tests := []struct {
		name          string
		ips           []string
		expectedIPv4s []string
		expectedIPv6s []string
	}{
		{
			name:          "nil slice",
			ips:           nil,
			expectedIPv4s: []string{},
			expectedIPv6s: []string{},
		},
		{
			name:          "empty slice",
			ips:           []string{},
			expectedIPv4s: []string{},
			expectedIPv6s: []string{},
		},
		{
			name:          "only ipv4",
			ips:           []string{"192.168.1.1", "10.0.0.1"},
			expectedIPv4s: []string{"192.168.1.1", "10.0.0.1"},
			expectedIPv6s: []string{},
		},
		{
			name:          "only ipv6",
			ips:           []string{"2001:db8::1", "fe80::1"},
			expectedIPv4s: []string{},
			expectedIPv6s: []string{"2001:db8::1", "fe80::1"},
		},
		{
			name:          "mixed ips",
			ips:           []string{"192.168.1.1", "2001:db8::1", "10.0.0.1", "fe80::1"},
			expectedIPv4s: []string{"192.168.1.1", "10.0.0.1"},
			expectedIPv6s: []string{"2001:db8::1", "fe80::1"},
		},
		{
			name:          "with invalid ips",
			ips:           []string{"192.168.1.1", "not-an-ip", "10.0.0.1", "2001:db8::1"},
			expectedIPv4s: []string{"192.168.1.1", "10.0.0.1"},
			expectedIPv6s: []string{"2001:db8::1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipv4s, ipv6s := filterIPsByFamily(tt.ips)
			if !reflect.DeepEqual(ipv4s, tt.expectedIPv4s) {
				t.Errorf("IPv4s mismatch:\n  got:  %v\n  want: %v", ipv4s, tt.expectedIPv4s)
			}
			if !reflect.DeepEqual(ipv6s, tt.expectedIPv6s) {
				t.Errorf("IPv6s mismatch:\n  got:  %v\n  want: %v", ipv6s, tt.expectedIPv6s)
			}
		})
	}
}
