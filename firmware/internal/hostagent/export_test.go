package hostagent

// SetToolURLForTest 让测试在起好 httptest 服务后再指定工具端点地址。
func SetToolURLForTest(m *Manager, url string) { m.tool = url }

// SetEngineForTest 换掉引擎（Codex 引擎的验收在装配好假主机之后才建得出来）。
func SetEngineForTest(m *Manager, e Engine) { m.engine = e }
