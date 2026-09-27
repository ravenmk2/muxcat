package nacos

import (
	"context"
	"encoding/json"
	"net/http"
)

// resolveVersion determines the Nacos major version. A pinned instance
// version wins; "auto" probes the 3.x-only public state endpoint: a 200
// JSON answer (or an auth challenge — 3.x admin APIs may require auth,
// while 2.x 404s unknown paths) means 3.x, anything else means 2.x. The
// result is cached on the client, so a command process probes at most
// once.
func (c *client) resolveVersion(ctx context.Context) (int, error) {
	switch c.versionPin {
	case "2":
		return 2, nil
	case "3":
		return 3, nil
	}
	r, err := c.request(ctx, http.MethodGet, "/nacos/v3/admin/core/state", nil, nil, nil)
	if err != nil {
		return 0, err
	}
	if r.status == http.StatusOK && looksLikeJSON(r.body) {
		return 3, nil
	}
	if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
		return 3, nil
	}
	return 2, nil
}

func looksLikeJSON(body []byte) bool {
	var v map[string]any
	return json.Unmarshal(body, &v) == nil
}

// serverVersion reads the server's version string, best-effort: a missing
// or unparsable answer yields "" and never fails the caller.
func (c *client) serverVersion(ctx context.Context) string {
	path := "/nacos/v1/console/server/state"
	if c.version == 3 {
		path = "/nacos/v3/admin/core/state"
	}
	r, err := c.send(ctx, http.MethodGet, path, nil, nil, nil)
	if err != nil {
		return ""
	}
	v, err := decodeData(r.body)
	if err != nil {
		return ""
	}
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return strOf(m, "version")
}
