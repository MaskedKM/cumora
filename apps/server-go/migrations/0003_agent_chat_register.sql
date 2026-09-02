-- 0003_agent_chat_register.sql —— #24 按受众切换语域:每员工"聊天体"
-- 开关(human-audience 会话中说人话),默认开;员工编辑界面可关。
ALTER TABLE participants ADD COLUMN chat_register boolean NOT NULL DEFAULT true;
