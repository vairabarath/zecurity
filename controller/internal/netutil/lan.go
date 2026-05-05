package netutil

import "net"

// DetectLANAddr returns (grpcAddr, httpAddr) built from the machine's outbound LAN IP.
// Falls back to localhost if the UDP probe fails.
func DetectLANAddr(grpcPort, httpPort string) (grpcAddr, httpAddr string) {
	ip := detectLANIP()
	return ip + ":" + grpcPort, ip + ":" + httpPort
}

// IsLocalhost reports whether addr (host:port or bare host) resolves to a loopback address.
// Treats "localhost", "127.x.x.x", and "::1" as loopback.
func IsLocalhost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DetectLANIP returns the machine's outbound LAN IP, or "localhost" on failure.
func DetectLANIP() string {
	return detectLANIP()
}

func detectLANIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return "localhost"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
