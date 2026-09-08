-- Cursor 透明代理不解析、限制或配置模型；模型选择由 cursor-agent 自行发现并保存。
UPDATE agent_accounts
   SET default_model = ''
 WHERE provider = 'cursor';
