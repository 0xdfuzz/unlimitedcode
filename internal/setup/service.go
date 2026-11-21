package setup

import (
	"net/http"
	"time"
)

// Health reports whether an UnlimitedCode daemon answers on addr.
func Health(addr string) bool {
	client := &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
