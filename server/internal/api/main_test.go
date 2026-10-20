package api

import (
	"errors"
	"net"
	"net/http"
	"os"
	"testing"
)

// TestMain keeps the api tests hermetic. The metadata service built by testDeps
// uses http.DefaultTransport, so without this a library scan would call the real
// Wikipedia, Wikidata and TVMaze APIs. That is slow enough on a connected
// machine to exceed waitForScan's deadline. Loopback (httptest servers) still works.
func TestMain(m *testing.M) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	http.DefaultTransport = offlineTransport{base: base}
	os.Exit(m.Run())
}

type offlineTransport struct{ base http.RoundTripper }

func (o offlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if host == "localhost" {
		return o.base.RoundTrip(req)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return o.base.RoundTrip(req)
	}
	return nil, errors.New("api tests are offline: refusing request to " + host)
}
