package handler

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"yingce/backend/internal/kernel"
	"yingce/backend/internal/service"

	"github.com/gin-gonic/gin"
)

const siteTrafficBodyLimit = 16 << 10

func RegisterSiteTrafficRoutes(r *gin.RouterGroup, svc *service.Service) {
	r.POST("/traffic/events", func(c *gin.Context) {
		// 固定1024个散列桶，不保存IP，也不让未淘汰的localRate map随匿名身份数增长；碰撞会共享额度。
		if !enforceRateLimit(c, "site-traffic:all", 3000, time.Minute) ||
			!enforceRateLimit(c, siteTrafficRateBucket(c.ClientIP()), 120, time.Minute) {
			return
		}
		if !siteTrafficOriginAllowed(c) {
			fail(c, http.StatusForbidden, errors.New("访问统计来源无效或未获允许"))
			return
		}
		mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mediaType != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
			fail(c, http.StatusUnsupportedMediaType, errors.New("访问统计只接受application/json UTF-8请求"))
			return
		}
		if c.Request.ContentLength > siteTrafficBodyLimit {
			fail(c, http.StatusRequestEntityTooLarge, errors.New("访问统计请求不能超过16KiB"))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, siteTrafficBodyLimit)
		// action 保留原JSON类型，区分缺省/空字符串与不属于合同的null。
		var wire struct {
			Events []struct {
				ID        string          `json:"id"`
				VisitorID string          `json:"visitorId"`
				SessionID string          `json:"sessionId"`
				Type      string          `json:"type"`
				Page      string          `json:"page"`
				Action    json.RawMessage `json:"action"`
			} `json:"events"`
		}
		decoder := json.NewDecoder(c.Request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&wire); err != nil {
			failSiteTrafficJSON(c, err)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			failSiteTrafficJSON(c, err)
			return
		}
		request := service.SiteTrafficBatchRequest{Events: make([]service.SiteTrafficEventRequest, 0, len(wire.Events))}
		for _, event := range wire.Events {
			action := ""
			if len(event.Action) > 0 {
				if strings.TrimSpace(string(event.Action)) == "null" || json.Unmarshal(event.Action, &action) != nil {
					fail(c, http.StatusBadRequest, errors.New("访问统计操作键必须为字符串"))
					return
				}
			}
			request.Events = append(request.Events, service.SiteTrafficEventRequest{
				ID: event.ID, VisitorID: event.VisitorID, SessionID: event.SessionID,
				Type: event.Type, Page: event.Page, Action: action,
			})
		}
		result, err := svc.RecordSiteTrafficEvents(c.Request.Context(), request)
		if err != nil {
			failService(c, err)
			return
		}
		ok(c, result)
	})
	r.GET("/admin/traffic/overview", func(c *gin.Context) {
		actor, err := currentUser(c, svc)
		if err != nil {
			failService(c, err)
			return
		}
		if err := svc.RequireAdmin(actor); err != nil {
			failService(c, err)
			return
		}
		query, err := url.ParseQuery(c.Request.URL.RawQuery)
		if err != nil {
			fail(c, http.StatusBadRequest, errors.New("访问统计日期参数无效"))
			return
		}
		for _, key := range []string{"from", "to"} {
			if values, present := query[key]; present && (len(values) != 1 || values[0] == "") {
				fail(c, http.StatusBadRequest, errors.New("访问统计日期参数无效"))
				return
			}
		}
		result, err := svc.AdminSiteTrafficOverview(c.Request.Context(), actor, service.SiteTrafficQuery{From: query.Get("from"), To: query.Get("to")})
		if err != nil {
			failService(c, err)
			return
		}
		ok(c, result)
	})
	r.GET("/admin/traffic/today", func(c *gin.Context) {
		actor, err := currentUser(c, svc)
		if err != nil {
			failService(c, err)
			return
		}
		if err := svc.RequireAdmin(actor); err != nil {
			failService(c, err)
			return
		}
		result, err := svc.AdminSiteTrafficToday(c.Request.Context(), actor)
		if err != nil {
			failService(c, err)
			return
		}
		ok(c, result)
	})
}

func failSiteTrafficJSON(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(c, http.StatusRequestEntityTooLarge, errors.New("访问统计请求不能超过16KiB"))
		return
	}
	// 不回显解析器中可能包含的未知字段或请求内容。
	fail(c, http.StatusBadRequest, errors.New("访问统计JSON无效或包含未知字段"))
}

func siteTrafficRateBucket(ip string) string {
	digest := sha256.Sum256([]byte(ip))
	bucket := (int(digest[0])<<8 | int(digest[1])) % 1024
	return "site-traffic:bucket:" + strconv.Itoa(bucket)
}

func parseSiteTrafficOrigin(value string) (*url.URL, bool) {
	origin, err := url.Parse(value)
	if err != nil || value == "" || strings.TrimSpace(value) != value ||
		(origin.Scheme != "http" && origin.Scheme != "https") || origin.Hostname() == "" || origin.User != nil ||
		origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Opaque != "" {
		return nil, false
	}
	if port := origin.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, false
		}
	}
	return origin, true
}

func siteTrafficOriginAllowed(c *gin.Context) bool {
	values := c.Request.Header.Values("Origin")
	if len(values) != 1 {
		return false
	}
	if _, valid := parseSiteTrafficOrigin(values[0]); !valid {
		return false
	}
	// 复用全局CORS已校验结果；这个公开写入口不把*或任意转发Host当作明确授权。
	if c.Writer.Header().Get("Access-Control-Allow-Origin") != values[0] {
		return false
	}
	normalizedOrigin, err := kernel.NormalizeOrigin(values[0])
	if err != nil {
		return false
	}
	for _, configured := range strings.Split(os.Getenv("CANVAS_CORS_ORIGINS"), ",") {
		allowed, err := kernel.NormalizeOrigin(strings.TrimSpace(configured))
		if err == nil && allowed == normalizedOrigin {
			return true
		}
	}
	// 与全局 CORS 共用同源判定：转发头重建 + 部署者声明的对外地址。
	// 这个公开写入口不把 * 或任意转发 Host 当作明确授权。
	return (kernel.ForwardedRequest{
		TLS:            c.Request.TLS != nil,
		RemoteAddr:     c.Request.RemoteAddr,
		Host:           c.Request.Host,
		ForwardedHost:  c.GetHeader("X-Forwarded-Host"),
		ForwardedProto: c.GetHeader("X-Forwarded-Proto"),
	}).SameOriginAsRequest(normalizedOrigin)
}
