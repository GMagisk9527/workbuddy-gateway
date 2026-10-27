package main

import (
	"errors"
	"sync/atomic"
	"time"
)

type downstreamFileConfig struct {
	WriteTimeoutSeconds int `json:"writeTimeoutSeconds"`
}

type serverFileConfig struct {
	ShutdownGraceSeconds      int `json:"shutdownGraceSeconds"`
	ShutdownCleanupSeconds    int `json:"shutdownCleanupSeconds"`
	RequestReadTimeoutSeconds int `json:"requestReadTimeoutSeconds"`
}

type serviceTimeoutConfig struct {
	write, read, grace, cleanup time.Duration
}

var serviceTimeouts atomic.Pointer[serviceTimeoutConfig]

func normalizeServiceTimeouts(d downstreamFileConfig, s serverFileConfig) (serviceTimeoutConfig, error) {
	if d.WriteTimeoutSeconds < 0 || d.WriteTimeoutSeconds > 3600 || s.ShutdownGraceSeconds < 0 || s.ShutdownGraceSeconds > 3600 || s.ShutdownCleanupSeconds < 0 || s.ShutdownCleanupSeconds > 300 || s.RequestReadTimeoutSeconds < 0 || s.RequestReadTimeoutSeconds > 3600 {
		return serviceTimeoutConfig{}, errors.New("downstream 写超时、server 停机等待和请求读取超时需为 0～3600 秒，清理等待需为 0～300 秒；0 使用默认值")
	}
	c := serviceTimeoutConfig{write: 30 * time.Second, read: 900 * time.Second, grace: 120 * time.Second, cleanup: 15 * time.Second}
	if d.WriteTimeoutSeconds > 0 {
		c.write = time.Duration(d.WriteTimeoutSeconds) * time.Second
	}
	if s.ShutdownGraceSeconds > 0 {
		c.grace = time.Duration(s.ShutdownGraceSeconds) * time.Second
	}
	if s.ShutdownCleanupSeconds > 0 {
		c.cleanup = time.Duration(s.ShutdownCleanupSeconds) * time.Second
	}
	if s.RequestReadTimeoutSeconds > 0 {
		c.read = time.Duration(s.RequestReadTimeoutSeconds) * time.Second
	}
	return c, nil
}

func currentServiceTimeouts() serviceTimeoutConfig {
	if c := serviceTimeouts.Load(); c != nil {
		return *c
	}
	c, _ := normalizeServiceTimeouts(downstreamFileConfig{}, serverFileConfig{})
	return c
}
