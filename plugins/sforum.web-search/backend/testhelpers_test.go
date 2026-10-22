package main

import (
	"testing"
)

// testOutbound 构造一个允许明文 http 与内网端点的出站执行器。
//
// 测试要打到 httptest 的 127.0.0.1，而这正是生产默认拒绝的地址；用配置开关
// 打开它（而不是在守卫里开一个「测试模式」后门），验证的仍然是同一条代码路径。
func testOutbound(t *testing.T) *outbound {
	t.Helper()
	config := DefaultConfig()
	config.AllowHTTP = true
	config.AllowPrivateEndpoint = true
	client, err := newProviderClient(config)
	if err != nil {
		t.Fatalf("build provider client: %v", err)
	}
	return &outbound{client: client, config: config}
}

// asFailure 断言错误是 *searchFailure，并把它交回给调用方做进一步断言。
func asFailure(t *testing.T, err error) *searchFailure {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a failure, got nil")
	}
	failure := asSearchFailure("", err)
	if failure == nil {
		t.Fatalf("expected a *searchFailure, got %T", err)
	}
	return failure
}
