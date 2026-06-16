package policy

import "testing"

func TestValidateRelayConfig(t *testing.T) {
	cases := []struct {
		name      string
		addr, spi string
		wantErr   bool
	}{
		{"both empty (disabled)", "", "", false},
		{"both set (enabled)", "relay.x:9093", "spiffe://td/relay/r1", false},
		{"addr without spiffe", "relay.x:9093", "", true},
		{"spiffe without addr", "", "spiffe://td/relay/r1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRelayConfig(c.addr, c.spi)
			if (err != nil) != c.wantErr {
				t.Fatalf("addr=%q spiffe=%q got err=%v want err=%v", c.addr, c.spi, err, c.wantErr)
			}
		})
	}
}
