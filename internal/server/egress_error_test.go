package server

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hirotomasato/autoclawpi/internal/db"
)

// TestIsEgressError 覆盖「哪些错误该触发 network cooldown」。
//
// 背景：节点挂了（proxy 不可达）时，坏号原本会被每次请求重试一遍再
// failover，拖慢所有请求。修复是给这类错误打短时 cooldown。
// 但必须只对**网络层**错误生效 —— 请求体错误不该连坐健康账号。
func TestIsEgressError(t *testing.T) {
	yes := []struct {
		name string
		err  error
	}{
		{"connection refused（mihomo 端口关）",
			errors.New(`request: Post "https://x/y": dial tcp 127.0.0.1:7902: connect: connection refused`)},
		{"proxyconnect（proxy 握手失败）",
			errors.New(`request: Post "https://x/y": proxyconnect tcp: dial tcp 127.0.0.1:7902: connect: connection refused`)},
		{"i/o timeout（节点无响应）",
			errors.New(`request: Post "https://x/y": dial tcp 203.0.113.10:11881: i/o timeout`)},
		{"context deadline exceeded",
			errors.New(`request: Post "https://x/y": context deadline exceeded`)},
		{"connection reset",
			errors.New(`request: Post "https://x/y": read tcp: connection reset by peer`)},
		{"no route to host",
			errors.New(`request: dial tcp: no route to host`)},
		{"network is unreachable",
			errors.New(`request: dial tcp: network is unreachable`)},
		{"unexpected EOF",
			errors.New(`request: Post "https://x/y": unexpected EOF`)},
		{"wrapped（errors.Is 风格包装）",
			fmt.Errorf("request: %w", errors.New("dial tcp 1.2.3.4:443: i/o timeout"))},
	}
	for _, tc := range yes {
		t.Run("yes/"+tc.name, func(t *testing.T) {
			if !isEgressError(tc.err) {
				t.Errorf("应判定为 egress 错误: %v", tc.err)
			}
		})
	}

	no := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"buat request（请求构造失败，不是网络）",
			errors.New(`buat request: parse url: invalid`)},
		{"context canceled（客户端取消）",
			errors.New(`request: Post "https://x/y": context canceled`)},
		{"上游业务错误（不是网络层）",
			errors.New(`upstream: 410004 账号已被封禁`)},
		{"空错误串", errors.New("")},
	}
	for _, tc := range no {
		t.Run("no/"+tc.name, func(t *testing.T) {
			if isEgressError(tc.err) {
				t.Errorf("不应判定为 egress 错误: %v", tc.err)
			}
		})
	}
}

// TestCooldownNetworkDuration 确认 network cooldown 是短时（会自动恢复）。
func TestCooldownNetworkDuration(t *testing.T) {
	d := db.CooldownDuration(db.CooldownNetwork)
	if d <= 0 {
		t.Fatalf("network cooldown 时长应为正数，实际 %v", d)
	}
	// 必须比 banned(24h) 短得多 —— 节点抖动是暂时现象
	if d >= 24*60*60*1e9 {
		t.Errorf("network cooldown 过长: %v", d)
	}
	// 但也不能太短，否则节点持续挂时会反复重试拖慢请求
	if d < 30*1e9 {
		t.Errorf("network cooldown 过短（会反复重试）: %v", d)
	}
	t.Logf("network cooldown = %v", d)
}

// TestCooldownNetworkReasonRegistered 确认常量已注册到时长表。
func TestCooldownNetworkReasonRegistered(t *testing.T) {
	if db.CooldownNetwork == "" {
		t.Fatal("CooldownNetwork 常量为空")
	}
	// 未注册的 reason 会 fallback 到 10min；network 必须走自己的值
	if db.CooldownDuration(db.CooldownNetwork) == 10*60*1e9 {
		t.Errorf("CooldownNetwork 似乎未注册（fallback 到默认 10min）")
	}
}
