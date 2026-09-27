package nacos

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ravenmk2/muxcat/internal/output"
)

// apiV2 adapts the Nacos 2.x OpenAPI: v2 config endpoints first with a v1
// fallback, v1 naming/console endpoints elsewhere. The public namespace is
// addressed by an empty namespace id on this API generation.
type apiV2 struct{ c *client }

// ns2 maps the public namespace to the empty id 2.x expects.
func ns2(namespace string) string {
	if namespace == "public" {
		return ""
	}
	return namespace
}

// ns2Display maps the empty namespace id back to public for display.
func ns2Display(namespace string) string {
	if namespace == "" {
		return "public"
	}
	return namespace
}

// blurPattern turns a plain filter into a blur-search pattern: Nacos blur
// search is wildcard-based (* and ?), not substring, so a bare filter is
// wrapped with * on both sides; patterns already carrying wildcards pass
// through unchanged.
func blurPattern(s string) string {
	if s == "" || strings.ContainsAny(s, "*?") {
		return s
	}
	return "*" + s + "*"
}

func (a apiV2) configGet(ctx context.Context, dataID, group, namespace string) (string, string, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v2/cs/config", url.Values{
		"dataId":      {dataID},
		"group":       {group},
		"namespaceId": {ns2(namespace)},
	}, nil, nil)
	if err != nil && r != nil && r.status == http.StatusNotFound {
		// v1 fallback: older 2.x servers without the v2 config endpoint.
		// A 404 here means the config itself does not exist and is
		// reported as-is.
		r1, err1 := a.c.send(ctx, http.MethodGet, "/nacos/v1/cs/configs", url.Values{
			"dataId": {dataID},
			"group":  {group},
			"tenant": {ns2(namespace)},
		}, nil, nil)
		if err1 != nil {
			return "", "", err1
		}
		return string(r1.body), "", nil
	}
	if err != nil {
		return "", "", err
	}
	v, err := decodeData(r.body)
	if err != nil {
		return "", "", err
	}
	if s, ok := v.(string); ok {
		return s, "", nil
	}
	return "", "", output.NewError(output.CodeQueryError,
		"unexpected config get response: "+truncate(string(r.body), 512), "")
}

// writeOK interprets a publish/delete response: the v2 envelope
// ({code:0,...,data:true}), or the v1 bare "true"/"false".
func writeOK(body []byte) error {
	v, err := decodeData(body)
	if err != nil {
		return err
	}
	switch t := v.(type) {
	case bool:
		if t {
			return nil
		}
	case string:
		if strings.TrimSpace(t) == "true" {
			return nil
		}
	}
	return output.NewError(output.CodeQueryError,
		"write rejected by server: "+truncate(string(body), 512), "")
}

func (a apiV2) configPublish(ctx context.Context, dataID, group, namespace, content, contentType string) error {
	params := url.Values{
		"dataId":      {dataID},
		"group":       {group},
		"namespaceId": {ns2(namespace)},
		"content":     {content},
	}
	if contentType != "" {
		params.Set("type", contentType)
	}
	r, err := a.c.send(ctx, http.MethodPost, "/nacos/v2/cs/config", params, nil, nil)
	if err != nil && r != nil && r.status == http.StatusNotFound {
		params.Set("tenant", params.Get("namespaceId"))
		params.Del("namespaceId")
		r1, err1 := a.c.send(ctx, http.MethodPost, "/nacos/v1/cs/configs", params, nil, nil)
		if err1 != nil {
			return err1
		}
		return writeOK(r1.body)
	}
	if err != nil {
		return err
	}
	return writeOK(r.body)
}

func (a apiV2) configDelete(ctx context.Context, dataID, group, namespace string) error {
	r, err := a.c.send(ctx, http.MethodDelete, "/nacos/v2/cs/config", url.Values{
		"dataId":      {dataID},
		"group":       {group},
		"namespaceId": {ns2(namespace)},
	}, nil, nil)
	if err != nil && r != nil && r.status == http.StatusNotFound {
		r1, err1 := a.c.send(ctx, http.MethodDelete, "/nacos/v1/cs/configs", url.Values{
			"dataId": {dataID},
			"group":  {group},
			"tenant": {ns2(namespace)},
		}, nil, nil)
		if err1 != nil {
			return err1
		}
		return writeOK(r1.body)
	}
	if err != nil {
		return err
	}
	return writeOK(r.body)
}

func (a apiV2) configList(ctx context.Context, dataID, group, namespace string, pageNo, pageSize int) (*configPage, error) {
	// The accurate search is the only 2.x list flavor that reliably
	// populates the item type — older servers (verified on 2.2.0) answer
	// blur items with a null type. The dataId filter is therefore applied
	// client-side, with the same wildcard semantics the server's blur
	// search has.
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v1/cs/configs", url.Values{
		"search":   {"accurate"},
		"dataId":   {""},
		"group":    {group},
		"tenant":   {ns2(namespace)},
		"pageNo":   {strconv.Itoa(pageNo)},
		"pageSize": {strconv.Itoa(pageSize)},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	v, err := decodeData(r.body)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, output.NewError(output.CodeQueryError,
			"unexpected config list response: "+truncate(string(r.body), 512), "")
	}
	page := &configPage{Total: intOf(m, "totalCount", "total")}
	pattern := blurPattern(dataID)
	for _, it := range itemsOf(m) {
		id := strOf(it, "dataId")
		if pattern != "" && !wildcardMatch(pattern, id) {
			continue
		}
		page.Items = append(page.Items, configItem{
			DataID:    id,
			Group:     strOf(it, "group"),
			Namespace: ns2Display(strOf(it, "tenant", "namespace", "namespaceId")),
			Type:      strOf(it, "type"),
		})
	}
	return page, nil
}

// configType looks up a config's server-recorded type via an exact
// (accurate) search. Best-effort: any failure yields "".
func (a apiV2) configType(ctx context.Context, dataID, group, namespace string) string {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v1/cs/configs", url.Values{
		"search":   {"accurate"},
		"dataId":   {dataID},
		"group":    {group},
		"tenant":   {ns2(namespace)},
		"pageNo":   {"1"},
		"pageSize": {"1"},
	}, nil, nil)
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
	for _, it := range itemsOf(m) {
		if strOf(it, "dataId") == dataID {
			return strOf(it, "type")
		}
	}
	return ""
}

// wildcardMatch mirrors the server-side blur search: * matches any run, a
// pattern without wildcards is an exact match.
func wildcardMatch(pattern, s string) bool {
	if pattern == "*" || pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, p := range parts {
		if p == "" {
			continue
		}
		idx := strings.Index(s[pos:], p)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 { // anchored prefix
			return false
		}
		pos += idx + len(p)
	}
	if last := parts[len(parts)-1]; last != "" && !strings.HasSuffix(s, last) {
		return false
	}
	return true
}

func (a apiV2) serviceList(ctx context.Context, namespace string, pageNo, pageSize int) (*servicePage, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v1/ns/service/list", url.Values{
		"pageNo":      {strconv.Itoa(pageNo)},
		"pageSize":    {strconv.Itoa(pageSize)},
		"namespaceId": {ns2(namespace)},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	v, err := decodeData(r.body)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, output.NewError(output.CodeQueryError,
			"unexpected service list response: "+truncate(string(r.body), 512), "")
	}
	page := &servicePage{Total: intOf(m, "count", "totalCount", "total")}
	// The 2.x listing carries plain service names in doms.
	for _, d := range anySlice(m["doms"]) {
		if s, ok := d.(string); ok {
			page.Services = append(page.Services, serviceItem{Name: s})
		}
	}
	return page, nil
}

func (a apiV2) serviceDetail(ctx context.Context, service, group, namespace string) (any, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v1/ns/service", url.Values{
		"serviceName": {service},
		"namespaceId": {ns2(namespace)},
		"groupName":   {group},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeData(r.body)
}

func (a apiV2) instanceList(ctx context.Context, service, group, namespace string) ([]instanceInfo, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v1/ns/instance/list", url.Values{
		"serviceName": {service},
		"namespaceId": {ns2(namespace)},
		"groupName":   {group},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	v, err := decodeData(r.body)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, output.NewError(output.CodeQueryError,
			"unexpected instance list response: "+truncate(string(r.body), 512), "")
	}
	return parseHosts(m["hosts"]), nil
}

func (a apiV2) namespaceList(ctx context.Context) ([]namespaceInfo, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v1/console/namespaces", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	v, err := decodeData(r.body)
	if err != nil {
		return nil, err
	}
	var out []namespaceInfo
	for _, it := range anySlice(v) {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, namespaceInfo{
			Namespace:   ns2Display(strOf(m, "namespace")),
			ShowName:    strOf(m, "namespaceShowName", "showName"),
			Quota:       intOf(m, "quota"),
			ConfigCount: intOf(m, "configCount"),
		})
	}
	return out, nil
}

// itemsOf extracts the page-items array of a listing response, tolerating
// both the 2.x (pageItems) and 3.x (pageItems/data) shapes.
func itemsOf(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, key := range []string{"pageItems", "items", "list"} {
		for _, it := range anySlice(m[key]) {
			if im, ok := it.(map[string]any); ok {
				out = append(out, im)
			}
		}
		if out != nil {
			return out
		}
	}
	return nil
}

// anySlice normalizes a decoded value to a slice.
func anySlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// parseHosts converts a decoded hosts array into instance rows.
func parseHosts(v any) []instanceInfo {
	var out []instanceInfo
	for _, it := range anySlice(v) {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		info := instanceInfo{
			IP:      strOf(m, "ip"),
			Port:    intOf(m, "port"),
			Healthy: m["healthy"] == true,
			Enabled: m["enabled"] == true,
		}
		switch w := m["weight"].(type) {
		case float64:
			info.Weight = w
		case string:
			_, _ = fmt.Sscan(w, &info.Weight)
		}
		out = append(out, info)
	}
	return out
}
