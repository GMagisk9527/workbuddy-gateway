package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"
)

func newAPIHandler() http.Handler {
	mux := http.NewServeMux()
	// 请求体解码只挂在真正消费 JSON body 的协议路由上。不能包住整个 mux：
	// /health、/ping、/ 免鉴权，/admin/probe 的鉴权在 mux 内部，若在这一层解码，
	// 匿名请求就能先触发一次大体积解压（压缩炸弹）。
	mux.HandleFunc("/v1/chat/completions", withRequestBodyDecode(handleChatCompletions))
	mux.HandleFunc("/chat/completions", withRequestBodyDecode(handleChatCompletions))
	mux.HandleFunc("/v1/responses", withRequestBodyDecode(handleResponses))
	mux.HandleFunc("/responses", withRequestBodyDecode(handleResponses))
	mux.HandleFunc("/v1/messages", withRequestBodyDecode(handleMessages))
	mux.HandleFunc("/v1/messages/count_tokens", withRequestBodyDecode(handleCountTokens))
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/usage", handleKeyUsage)
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/ping", handleHealth)
	// 探测接口：先过管理员鉴权与回环检查，再解码请求体。
	mux.HandleFunc("/admin/probe", probeAdminOnly(withRequestBodyDecode(handleAdminProbe)))
	mux.HandleFunc("/", handleIndex)
	return requestAuditMiddleware(corsMiddleware(authMiddleware(keyAdmissionMiddleware(mux))))
}

func prepareGatewayServers() ([]*http.Server, []net.Listener, error) {
	return prepareGatewayServersWithLifecycle(newGatewayLifecycle())
}

func prepareGatewayServersWithLifecycle(life *gatewayLifecycle) ([]*http.Server, []net.Listener, error) {
	if err := webUIPreflightError(); err != nil {
		return nil, nil, err
	}
	servers := []*http.Server{{
		Addr: net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.Port)), Handler: life.handler(newAPIHandler()),
		ReadTimeout: 120 * time.Second,
		// 保留模型流无总写入时限，不把管理端限制扩散到模型 API。
		WriteTimeout: 0,
	}}
	if cfg.WebUI {
		servers = append(servers, &http.Server{
			Addr: net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.WebPort)), Handler: life.handler(newWebUIHandler()),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		})
	}
	trace := newTraceID()
	var listeners []net.Listener
	for i, server := range servers {
		server.BaseContext = func(net.Listener) context.Context { return life.ctx }
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			log.Printf("[监听初始化] traceId=%s 地址=%s 结果=失败 原因=%v 业务影响=已释放先前监听，未启动后台业务", trace, server.Addr, err)
			return nil, nil, fmt.Errorf("监听 %s 失败: %w", server.Addr, err)
		}
		listeners = append(listeners, listener)
		log.Printf("[监听初始化] traceId=%s 地址=%s 管理端=%t 结果=绑定成功", trace, server.Addr, i == 1)
	}
	return servers, listeners, nil
}
