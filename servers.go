package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
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

const apiReadHeaderTimeout = 15 * time.Second

func prepareGatewayServersWithLifecycle(life *gatewayLifecycle) ([]*http.Server, []net.Listener, error) {
	if err := webUIPreflightError(); err != nil {
		return nil, nil, err
	}
	timeouts := currentServiceTimeouts()
	servers := []*http.Server{{
		Addr: net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.Port)), Handler: life.handler(newAPIHandler()),
		ReadHeaderTimeout: apiReadHeaderTimeout,
		ReadTimeout:       timeouts.read,
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

type gatewayHTTPSlot struct {
	server   *http.Server
	listener net.Listener
	web      bool
}

type gatewayServerManager struct {
	mu        sync.Mutex
	installed bool
	addr      string
	api       *gatewayHTTPSlot
	web       *gatewayHTTPSlot
	retired   []*http.Server
	errs      chan<- error
	life      *gatewayLifecycle
}

var gatewayServers gatewayServerManager

func newGatewayHTTPSlot(addr string, web bool, life *gatewayLifecycle) *gatewayHTTPSlot {
	var handler http.Handler
	var server *http.Server
	if web {
		handler = newWebUIHandler()
		server = &http.Server{
			Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		}
	} else {
		handler = newAPIHandler()
		server = &http.Server{
			Addr: addr, Handler: handler, ReadHeaderTimeout: apiReadHeaderTimeout,
			ReadTimeout: currentServiceTimeouts().read, WriteTimeout: 0,
		}
	}
	if life != nil {
		server.Handler = life.handler(handler)
		server.BaseContext = func(net.Listener) context.Context { return life.ctx }
	}
	return &gatewayHTTPSlot{server: server, web: web}
}

// register adopts listeners that were already bound by prepareGatewayServers.
func (m *gatewayServerManager) register(servers []*http.Server, listeners []net.Listener, life *gatewayLifecycle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.installed, m.addr = true, cfg.Addr
	m.life = life
	m.api, m.web, m.retired = nil, nil, nil
	for i, server := range servers {
		slot := &gatewayHTTPSlot{server: server, listener: listeners[i], web: i == 1}
		if slot.web {
			m.web = slot
		} else {
			m.api = slot
		}
	}
}

func (m *gatewayServerManager) start(errs chan<- error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errs = errs
	m.serveLocked(m.api)
	m.serveLocked(m.web)
}

func (m *gatewayServerManager) serveLocked(slot *gatewayHTTPSlot) {
	if slot == nil {
		return
	}
	errs := m.errs
	server, listener := slot.server, slot.listener
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			if errs != nil {
				select {
				case errs <- fmt.Errorf("监听 %s 服务异常: %w", server.Addr, err):
				default:
				}
			}
		}
	}()
}

func (m *gatewayServerManager) ports() (int, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.installed {
		return cfg.Port, cfg.WebPort, false
	}
	apiPort, webPort := 0, 0
	if m.api != nil {
		apiPort, _ = strconv.Atoi(portOf(m.api.listener.Addr()))
	}
	if m.web != nil {
		webPort, _ = strconv.Atoi(portOf(m.web.listener.Addr()))
	}
	return apiPort, webPort, true
}

func portOf(addr net.Addr) string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "0"
	}
	return port
}

// apply swaps changed listeners without closing established connections.
// It validates and binds every new listener before retiring any old one.
func (m *gatewayServerManager) apply(g gatewayFileConfig, apiPort int, webEnabled bool) (int, int, error) {
	return m.applyWith(g, apiPort, webEnabled, nil)
}

// beforeCommit may prepare another fallible runtime resource (debug logging).
// Run it only after all new ports bind, but before publishing/retiring listeners.
func (m *gatewayServerManager) applyWith(g gatewayFileConfig, apiPort int, webEnabled bool, beforeCommit func() error) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.life != nil && m.life.isDraining() {
		return 0, 0, errors.New("网关正在排空，不能热切换监听端口")
	}
	if !m.installed {
		if beforeCommit != nil {
			if err := beforeCommit(); err != nil {
				return cfg.Port, cfg.WebPort, err
			}
		}
		return cfg.Port, cfg.WebPort, nil
	}
	currentAPI, currentWeb := 0, 0
	if m.api != nil {
		currentAPI, _ = strconv.Atoi(portOf(m.api.listener.Addr()))
	}
	if m.web != nil {
		currentWeb, _ = strconv.Atoi(portOf(m.web.listener.Addr()))
	}
	targetWeb := 0
	if webEnabled {
		targetWeb = g.WebPort
	}
	if apiPort == currentAPI && targetWeb == currentWeb {
		if beforeCommit != nil {
			if err := beforeCommit(); err != nil {
				return currentAPI, currentWeb, err
			}
		}
		return currentAPI, currentWeb, nil
	}
	// Swapping the two live ports cannot be pre-bound. Refuse rather than take
	// every listener offline; users can switch through a temporary third port.
	if apiPort == currentWeb || targetWeb != 0 && targetWeb == currentAPI {
		return currentAPI, currentWeb, errors.New("新端口与本进程另一现有监听正在互换，请先改到临时端口再切换")
	}
	newAPI, newWeb := m.api, m.web
	opened := []net.Listener{}
	closeOpened := func() {
		for _, listener := range opened {
			_ = listener.Close()
		}
	}
	if apiPort != currentAPI {
		listener, err := net.Listen("tcp", net.JoinHostPort(m.addr, strconv.Itoa(apiPort)))
		if err != nil {
			return currentAPI, currentWeb, fmt.Errorf("新 API 监听 %s 绑定失败: %w", net.JoinHostPort(m.addr, strconv.Itoa(apiPort)), err)
		}
		opened = append(opened, listener)
		slot := newGatewayHTTPSlot(listener.Addr().String(), false, m.life)
		slot.listener = listener
		newAPI = slot
	}
	if targetWeb != 0 && targetWeb != currentWeb {
		listener, err := net.Listen("tcp", net.JoinHostPort(m.addr, strconv.Itoa(targetWeb)))
		if err != nil {
			closeOpened()
			return currentAPI, currentWeb, fmt.Errorf("新 Web 监听 %s 绑定失败: %w", net.JoinHostPort(m.addr, strconv.Itoa(targetWeb)), err)
		}
		opened = append(opened, listener)
		slot := newGatewayHTTPSlot(listener.Addr().String(), true, m.life)
		slot.listener = listener
		newWeb = slot
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			closeOpened()
			return currentAPI, currentWeb, err
		}
	}
	oldAPI, oldWeb := m.api, m.web
	m.api, m.web = newAPI, newWeb
	if newAPI != oldAPI {
		m.serveLocked(newAPI)
	}
	if newWeb != oldWeb && newWeb != nil {
		m.serveLocked(newWeb)
	}
	// Close only listeners. Existing HTTP connections and active model streams
	// continue to be served by their original server goroutines.
	for _, old := range []*gatewayHTTPSlot{oldAPI, oldWeb} {
		if old == nil || old == newAPI || old == newWeb {
			continue
		}
		_ = old.listener.Close()
		m.retired = append(m.retired, old.server)
	}
	apiOut, webOut := apiPort, targetWeb
	if newAPI != nil {
		apiOut, _ = strconv.Atoi(portOf(newAPI.listener.Addr()))
	}
	if newWeb != nil {
		webOut, _ = strconv.Atoi(portOf(newWeb.listener.Addr()))
	}
	return apiOut, webOut, nil
}

// Detach every server, including retired listeners with established requests.
// Production drains this snapshot through the shared lifecycle and WAL cleanup.
func (m *gatewayServerManager) takeServers() []*http.Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	servers := append([]*http.Server(nil), m.retired...)
	for _, slot := range []*gatewayHTTPSlot{m.api, m.web} {
		if slot != nil {
			servers = append(servers, slot.server)
		}
	}
	m.installed, m.api, m.web, m.retired = false, nil, nil, nil
	m.life, m.errs = nil, nil
	return servers
}

func (m *gatewayServerManager) shutdown(ctx context.Context) {
	var wg sync.WaitGroup
	for _, server := range m.takeServers() {
		wg.Add(1)
		go func(server *http.Server) {
			defer wg.Done()
			if err := server.Shutdown(ctx); err != nil {
				_ = server.Close()
			}
		}(server)
	}
	wg.Wait()
}
