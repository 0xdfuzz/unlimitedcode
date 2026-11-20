package setup

import (
	"fmt"
	"net/http"
	"time"
)

// Health reports whether a Kestrel daemon answers on addr.
func Health(addr string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// WaitHealthy polls until the daemon answers or the timeout expires.
func WaitHealthy(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if Health(addr) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not answer on %s", addr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// launcherVBS runs the daemon without a console window at logon.
func launcherVBS(exe string) string {
	return "Set shell = CreateObject(\"WScript.Shell\")\r\n" +
		"shell.Run \"\"\"" + exe + "\"\" daemon\", 0, False\r\n"
}
