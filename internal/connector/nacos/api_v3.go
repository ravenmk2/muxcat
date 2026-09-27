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

// apiV3 adapts the Nacos 3.x OpenAPI (server port only, never the console
// port). All answers are envelopes {code,message,data}; decodeData
// unwraps them. Parameter names are groupName/namespaceId (the 2.x "group"
// is rejected with 400), and the namespace id is always passed explicitly
// — the admin side misbehaves on an empty value.
type apiV3 struct{ c *client }

func (a apiV3) configGet(ctx context.Context, dataID, group, namespace string) (string, string, error) {
	// The client-facing read endpoint works anonymously.
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v3/client/cs/config", url.Values{
		"dataId":      {dataID},
		"groupName":   {group},
		"namespaceId": {namespace},
	}, nil, nil)
	if err != nil {
		return "", "", err
	}
	v, err := decodeData(r.body)
	if err != nil {
		// A missing config is HTTP 200 with envelope code 20004 — map it
		// to the same not-found shape as the 2.x 404.
		if msg := output.ToError(err).Message; strings.Contains(msg, "20004") || strings.Contains(msg, "resource not found") {
			return "", "", configNotFound(dataID, group, namespace)
		}
		return "", "", err
	}
	// The 3.x envelope data is an object carrying the content and its
	// server-reported format (not a bare string like 2.x).
	switch d := v.(type) {
	case string:
		return d, "", nil
	case map[string]any:
		if s, ok := d["content"].(string); ok {
			return s, strOf(d, "contentType", "type"), nil
		}
	}
	return "", "", output.NewError(output.CodeQueryError,
		"unexpected config get response: "+truncate(string(r.body), 512), "")
}

// configNotFound is the shared missing-config error: QUERY_ERROR with the
// addressing hint (same classification as a 404).
func configNotFound(dataID, group, namespace string) *output.Error {
	return output.NewError(output.CodeQueryError,
		fmt.Sprintf("config not found: %s (%s, %s)", dataID, group, namespace),
		"check the addressing triple: dataId, group (-g, default DEFAULT_GROUP) and namespace (--namespace, default public)")
}

func (a apiV3) configPublish(ctx context.Context, dataID, group, namespace, content, contentType string) error {
	params := url.Values{
		"dataId":      {dataID},
		"groupName":   {group},
		"namespaceId": {namespace},
		"content":     {content},
	}
	if contentType != "" {
		params.Set("type", contentType)
	}
	r, err := a.c.send(ctx, http.MethodPost, "/nacos/v3/admin/cs/config", params, nil, nil)
	if err != nil {
		return err
	}
	return writeOK(r.body)
}

func (a apiV3) configDelete(ctx context.Context, dataID, group, namespace string) error {
	r, err := a.c.send(ctx, http.MethodDelete, "/nacos/v3/admin/cs/config", url.Values{
		"dataId":      {dataID},
		"groupName":   {group},
		"namespaceId": {namespace},
	}, nil, nil)
	if err != nil {
		return err
	}
	return writeOK(r.body)
}

func (a apiV3) configList(ctx context.Context, dataID, group, namespace string, pageNo, pageSize int) (*configPage, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v3/admin/cs/config/list", url.Values{
		"search":      {"blur"},
		"dataId":      {blurPattern(dataID)},
		"groupName":   {group},
		"namespaceId": {namespace},
		"pageNo":      {strconv.Itoa(pageNo)},
		"pageSize":    {strconv.Itoa(pageSize)},
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
	for _, it := range itemsOf(m) {
		page.Items = append(page.Items, configItem{
			DataID:    strOf(it, "dataId"),
			Group:     strOf(it, "groupName", "group"),
			Namespace: strOf(it, "namespaceId", "namespace", "tenant"),
			Type:      strOf(it, "type"),
		})
	}
	return page, nil
}

func (a apiV3) serviceList(ctx context.Context, namespace string, pageNo, pageSize int) (*servicePage, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v3/admin/ns/service/list", url.Values{
		"pageNo":      {strconv.Itoa(pageNo)},
		"pageSize":    {strconv.Itoa(pageSize)},
		"namespaceId": {namespace},
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
	for _, it := range append(append(anySlice(m["serviceList"]), anySlice(m["doms"])...), itemsOfAny(m)...) {
		switch s := it.(type) {
		case string:
			page.Services = append(page.Services, serviceItem{Name: s})
		case map[string]any:
			page.Services = append(page.Services, serviceItem{
				Name:  strOf(s, "name", "serviceName"),
				Group: strOf(s, "groupName", "group"),
			})
		}
	}
	return page, nil
}

// itemsOfAny returns the page-items array of a listing response as a raw
// slice (entries may be strings or objects).
func itemsOfAny(m map[string]any) []any {
	for _, key := range []string{"pageItems", "items", "list"} {
		if s := anySlice(m[key]); s != nil {
			return s
		}
	}
	return nil
}

func (a apiV3) serviceDetail(ctx context.Context, service, group, namespace string) (any, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v3/admin/ns/service", url.Values{
		"serviceName": {service},
		"groupName":   {group},
		"namespaceId": {namespace},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeData(r.body)
}

func (a apiV3) instanceList(ctx context.Context, service, group, namespace string) ([]instanceInfo, error) {
	// The client-facing read endpoint works anonymously.
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v3/client/ns/instance/list", url.Values{
		"serviceName": {service},
		"groupName":   {group},
		"namespaceId": {namespace},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	v, err := decodeData(r.body)
	if err != nil {
		return nil, err
	}
	// The 3.x instance list data is a bare hosts array.
	if hosts, ok := v.([]any); ok {
		return parseHosts(hosts), nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, output.NewError(output.CodeQueryError,
			"unexpected instance list response: "+truncate(string(r.body), 512), "")
	}
	return parseHosts(m["hosts"]), nil
}

func (a apiV3) namespaceList(ctx context.Context) ([]namespaceInfo, error) {
	r, err := a.c.send(ctx, http.MethodGet, "/nacos/v3/admin/core/namespace/list", nil, nil, nil)
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
