package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
)

func configurationApplicationStatus(path string, out io.Writer) {
	c, err := gateway.LoadConfig(path)
	if err != nil {
		fmt.Fprintln(out, "Configuration saved; automatic application pending (configuration is not readable).")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	key, err := (gateway.MacOSKeychain{}).Lookup(ctx, c.AccessTokenKeychain)
	if err == nil {
		host, port, splitErr := net.SplitHostPort(c.ListenAddr)
		if host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		if splitErr == nil {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/tidemux/config-status", nil)
			req.Header.Set("Authorization", "Bearer "+key)
			client := &http.Client{Timeout: 750 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			response, requestErr := client.Do(req)
			if requestErr == nil {
				defer response.Body.Close()
				var status struct {
					Status    string `json:"status"`
					Revision  string `json:"applied_revision"`
					ErrorCode string `json:"error_code"`
				}
				if response.StatusCode == 200 && json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&status) == nil {
					if status.Status == "applied" && status.Revision == gateway.ConfigRevision(path) {
						fmt.Fprintln(out, "Configuration applied by the running gateway.")
						return
					}
					fmt.Fprintln(out, "Configuration saved; automatic application pending. The gateway polls every second; each validation attempt takes at most 10 seconds.")
					if status.ErrorCode != "" {
						fmt.Fprintln(out, "The last valid configuration remains active; check the gateway config_reload events for the failure code.")
					}
					return
				}
			}
		}
	}
	fmt.Fprintln(out, "Configuration saved; automatic application pending (no running gateway acknowledged this configuration). A running gateway watches this file; otherwise it is loaded at startup.")
}
