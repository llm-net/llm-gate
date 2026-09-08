ALTER TABLE usage_hourly ADD COLUMN unavailable_requests INTEGER NOT NULL DEFAULT 0;
UPDATE usage_hourly SET unavailable_requests = MAX(0, requests - errors - rejected_requests)
 WHERE entry = 'cursor_agent'
   AND model_name IN ('Cursor Agent 对话', 'agent.v1.AgentService/Run', 'agent.v1.AgentService/RunSSE');
