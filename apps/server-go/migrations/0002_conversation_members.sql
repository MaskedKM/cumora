-- 0002_conversation_members.sql —— #137 membership 规范化(三步走之一:
-- 建表+回填+影子同步;读路径切换与 withSeqscanOff 退役在后续 PR)。
--
-- conversations.members jsonb 保持写入侧唯一事实源(全部 14 条写入
-- SQL、6 个包——conversations 域 4、runtime cliUpdateMembers 咽喉 +
-- cli_tools 建会话 ×2、email 线程 union + 建会话、onboard 建会话 ×2 +
-- join.go 成员追加、agents/invitations 各 1——零改动);
-- 本迁移建立规范化表 conversation_members 并由触发器同步:与父写
-- 同事务、原子、无应用侧漂移面,连直接 SQL 写 members 的路径(邮件
-- 线程成员 union、测试种行)也一并覆盖。
--
-- 为什么用触发器而非 Go 侧双写:写入点分散在 3 个包 12 处,Go 双写
-- = 12 次事务缝合 + 12 个漏写机会;单一 DDL 咽喉把这些全部归零。
-- GIN containment 计划不稳(参数化 @> 无统计)是本票动机,读侧仍按
-- 现状走 jsonb,下一张 PR 切 (participant_id, conversation_id) 索引。

CREATE TABLE conversation_members (
    conversation_id text NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    participant_id  text NOT NULL,
    PRIMARY KEY (conversation_id, participant_id)
);

-- "我的会话"读路径主索引:participant 前导;PK 尾列即 conversation_id,
-- join conversations 后按 company 过滤。
CREATE INDEX idx_conversation_members_participant
    ON conversation_members (participant_id, conversation_id);

-- 回填:存量行展开入表。ON CONFLICT 防御 jsonb 数组内含重复 id 的
-- legacy 行(jsonb 允许重复,PK 不允许)。
INSERT INTO conversation_members (conversation_id, participant_id)
SELECT c.id, m
  FROM conversations c,
       jsonb_array_elements_text(c.members) AS m
ON CONFLICT DO NOTHING;

-- 影子表维护:members 写入(或行插入/删除)时全量重建该会话的成员
-- 行。成员数组规模是群聊成员数(个位到几十),整行 DELETE+INSERT
-- 远比差分简单可靠。
CREATE OR REPLACE FUNCTION conversation_members_sync() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM conversation_members WHERE conversation_id = OLD.id;
        RETURN OLD;
    END IF;
    DELETE FROM conversation_members WHERE conversation_id = NEW.id;
    INSERT INTO conversation_members (conversation_id, participant_id)
    SELECT NEW.id, m FROM jsonb_array_elements_text(NEW.members) AS m
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_conversations_members_sync
    AFTER INSERT OR UPDATE OF members OR DELETE ON conversations
    FOR EACH ROW EXECUTE FUNCTION conversation_members_sync();
