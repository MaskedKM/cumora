-- Go 侧迁移基线(#51 起,#70 固化全量)。
-- 历史说明:本文件曾是空占位——schema 一直由 TS 迁移器(已退役 TS
-- server 树的 db/migrate.ts,goose)管辖;#70 TS 退役时,其最终 schema 经 pg_dump 从
-- 正典测试库固化于此,自此 Go 迁移自足(全新库一步到正典)。
-- 已记账过旧版 0001 的库(生产、本地 cumora_test)不会重跑本文件——
-- 它们的 schema 本就由 TS 迁移器铺就,与本基线等价。
-- 后续变更:追加 0002_*.sql 增量,勿改本文件。
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;

CREATE TABLE public.agent_autonomy (
    user_id text NOT NULL,
    agent_id text NOT NULL,
    threshold real DEFAULT 0.6 NOT NULL,
    pulled integer DEFAULT 0 NOT NULL,
    led integer DEFAULT 0 NOT NULL,
    dissolved integer DEFAULT 0 NOT NULL,
    company_id text
);

CREATE TABLE public.agent_climate (
    agent_id text NOT NULL,
    about_id text NOT NULL,
    company_id text DEFAULT 'personal'::text NOT NULL,
    affinity real DEFAULT 0 NOT NULL,
    trust real DEFAULT 0 NOT NULL,
    last_note text DEFAULT ''::text NOT NULL,
    history jsonb DEFAULT '[]'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.agent_events (
    id text NOT NULL,
    run_id text NOT NULL,
    agent_id text NOT NULL,
    company_id text,
    kind text NOT NULL,
    level text DEFAULT 'info'::text NOT NULL,
    title text NOT NULL,
    data jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.agent_log (
    id text NOT NULL,
    agent_id text NOT NULL,
    kind text NOT NULL,
    body text NOT NULL,
    ref jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    company_id text
);

CREATE TABLE public.agent_memory (
    id text NOT NULL,
    agent_id text NOT NULL,
    kind text NOT NULL,
    about text,
    body text NOT NULL,
    source jsonb,
    pinned boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    company_id text
);

CREATE TABLE public.agent_runs (
    id text NOT NULL,
    agent_id text NOT NULL,
    company_id text,
    trigger jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text DEFAULT 'running'::text NOT NULL,
    stage text,
    summary text,
    error text,
    input_message_ids jsonb DEFAULT '[]'::jsonb NOT NULL,
    inbox_count integer DEFAULT 0 NOT NULL,
    tool_call_count integer DEFAULT 0 NOT NULL,
    token_count integer DEFAULT 0 NOT NULL,
    fingerprint text,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    input_tokens integer DEFAULT 0 NOT NULL,
    cached_input_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_tokens integer DEFAULT 0 NOT NULL,
    output_tokens integer DEFAULT 0 NOT NULL,
    cost_usd double precision DEFAULT 0 NOT NULL,
    cost_estimated boolean DEFAULT true NOT NULL,
    model text
);

CREATE TABLE public.agent_tasks (
    id text NOT NULL,
    agent_id text NOT NULL,
    title text NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    due_at timestamp with time zone,
    ref jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    company_id text
);

CREATE TABLE public.agent_triages (
    id text NOT NULL,
    agent_id text NOT NULL,
    company_id text,
    source text NOT NULL,
    model text,
    actionable boolean DEFAULT false NOT NULL,
    reason text,
    input_tokens integer DEFAULT 0 NOT NULL,
    cached_input_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_tokens integer DEFAULT 0 NOT NULL,
    output_tokens integer DEFAULT 0 NOT NULL,
    cost_usd double precision DEFAULT 0 NOT NULL,
    cost_estimated boolean DEFAULT true NOT NULL,
    measured boolean DEFAULT true NOT NULL,
    run_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.agent_workspace (
    agent_id text NOT NULL,
    path text NOT NULL,
    body text DEFAULT ''::text NOT NULL,
    meta jsonb,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    company_id text,
    embedding public.vector(1536)
);

CREATE TABLE public.app_settings (
    key text NOT NULL,
    value jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text
);

CREATE TABLE public.audit_events (
    id bigint NOT NULL,
    user_id text,
    company_id text,
    ip text,
    user_agent text,
    kind text NOT NULL,
    detail jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.audit_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.audit_events_id_seq OWNED BY public.audit_events.id;

CREATE TABLE public.auth_attempts (
    id bigint NOT NULL,
    email text,
    ip text,
    success boolean NOT NULL,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.auth_attempts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.auth_attempts_id_seq OWNED BY public.auth_attempts.id;

CREATE TABLE public.board_card_comments (
    id text NOT NULL,
    card_id text NOT NULL,
    author_id text NOT NULL,
    body text NOT NULL,
    mentions jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.board_cards (
    id text NOT NULL,
    board_id text NOT NULL,
    column_id text NOT NULL,
    title text NOT NULL,
    description text,
    "position" double precision DEFAULT 0 NOT NULL,
    assignee_id text,
    mentions jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.board_columns (
    id text NOT NULL,
    board_id text NOT NULL,
    title text NOT NULL,
    "position" double precision DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.board_mention_reads (
    user_id text NOT NULL,
    last_read_at timestamp with time zone DEFAULT '1970-01-01 00:00:00+00'::timestamp with time zone NOT NULL
);

CREATE TABLE public.boards (
    id text NOT NULL,
    company_id text NOT NULL,
    title text NOT NULL,
    description text,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.calendar_dispatches (
    id text NOT NULL,
    event_id text NOT NULL,
    company_id text NOT NULL,
    scheduled_for timestamp with time zone NOT NULL,
    dispatched_at timestamp with time zone DEFAULT now() NOT NULL,
    status text DEFAULT 'dispatched'::text NOT NULL,
    conversation_id text,
    message_id text,
    error text
);

CREATE TABLE public.calendar_events (
    id text NOT NULL,
    company_id text NOT NULL,
    created_by text NOT NULL,
    kind text DEFAULT 'personal'::text NOT NULL,
    title text NOT NULL,
    description text,
    assignee_id text,
    target_conversation_id text,
    agent_prompt text,
    start_at timestamp with time zone NOT NULL,
    end_at timestamp with time zone,
    all_day boolean DEFAULT false NOT NULL,
    recurrence jsonb,
    status text DEFAULT 'active'::text NOT NULL,
    last_fired_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    reminder_minutes_before integer,
    reminder_channel text,
    is_private boolean DEFAULT false NOT NULL
);

CREATE TABLE public.calendar_reminders (
    id text NOT NULL,
    event_id text NOT NULL,
    company_id text NOT NULL,
    scheduled_for timestamp with time zone NOT NULL,
    fired_at timestamp with time zone DEFAULT now() NOT NULL,
    channel text NOT NULL,
    recipients jsonb DEFAULT '[]'::jsonb NOT NULL,
    status text DEFAULT 'sent'::text NOT NULL,
    error text
);

CREATE TABLE public.companies (
    id text NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    owner_user_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    starter_seeded_at timestamp with time zone,
    starter_dms_seeded_at timestamp with time zone,
    all_hands_conversation_id text,
    all_hands_seeded_at timestamp with time zone,
    pair_token text
);

CREATE TABLE public.company_invitations (
    token_hash text NOT NULL,
    company_id text NOT NULL,
    invited_by text NOT NULL,
    email text,
    role text DEFAULT 'member'::text NOT NULL,
    note text,
    max_uses integer DEFAULT 1 NOT NULL,
    use_count integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    last_accepted_at timestamp with time zone,
    last_accepted_by text
);

CREATE TABLE public.company_members (
    company_id text NOT NULL,
    user_id text NOT NULL,
    role text DEFAULT 'member'::text NOT NULL,
    joined_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.computers (
    id text NOT NULL,
    company_id text NOT NULL,
    owner_user_id text,
    name text NOT NULL,
    kind text NOT NULL,
    available_engines jsonb DEFAULT '[]'::jsonb NOT NULL,
    status text DEFAULT 'offline'::text NOT NULL,
    last_seen_at timestamp with time zone,
    credential_hash text,
    paired_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    daemon_version text,
    daemon_supervised boolean,
    pair_token text
);

CREATE TABLE public.convene_sessions (
    id text NOT NULL,
    conversation_id text NOT NULL,
    title text NOT NULL,
    flair text,
    started_by text NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    ended_at timestamp with time zone,
    state text DEFAULT 'live'::text NOT NULL,
    company_id text
);

CREATE TABLE public.convene_transcript (
    id text NOT NULL,
    session_id text NOT NULL,
    author_id text NOT NULL,
    kind text NOT NULL,
    body text NOT NULL,
    sequence integer NOT NULL,
    decision jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    company_id text
);

CREATE TABLE public.convening_info (
    conversation_id text NOT NULL,
    pulled_by_id text NOT NULL,
    pulled_at timestamp with time zone DEFAULT now() NOT NULL,
    headline_lead text NOT NULL,
    headline_tail text NOT NULL,
    subhead text NOT NULL,
    who_and_why jsonb NOT NULL,
    evidence jsonb NOT NULL,
    asks jsonb NOT NULL,
    trigger jsonb NOT NULL,
    reasoning jsonb NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    company_id text
);

CREATE TABLE public.conversation_counters (
    conversation_id text NOT NULL,
    next_sequence integer DEFAULT 1 NOT NULL
);

CREATE TABLE public.conversation_mutes (
    user_id text NOT NULL,
    conversation_id text NOT NULL,
    muted_at timestamp with time zone DEFAULT now() NOT NULL,
    muted_until timestamp with time zone
);

CREATE TABLE public.conversation_reads (
    user_id text NOT NULL,
    conversation_id text NOT NULL,
    last_read_at timestamp with time zone DEFAULT now() NOT NULL,
    last_read_message_id text DEFAULT ''::text NOT NULL,
    company_id text
);

CREATE TABLE public.conversations (
    id text NOT NULL,
    kind text NOT NULL,
    title text NOT NULL,
    subtitle text,
    members jsonb NOT NULL,
    pinned boolean DEFAULT false NOT NULL,
    tag text,
    pulled_by jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    topic text,
    company_id text,
    project_id text
);

CREATE TABLE public.document_mentions (
    id text NOT NULL,
    document_id text NOT NULL,
    company_id text NOT NULL,
    mentioner_id text NOT NULL,
    mentioned_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.document_snapshots (
    document_id text NOT NULL,
    state_bytes bytea NOT NULL,
    snapshot_at_update_id bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.document_updates (
    id bigint NOT NULL,
    document_id text NOT NULL,
    author_id text NOT NULL,
    update_bytes bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.document_updates_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.document_updates_id_seq OWNED BY public.document_updates.id;

CREATE TABLE public.documents (
    id text NOT NULL,
    company_id text NOT NULL,
    title text DEFAULT 'Untitled'::text NOT NULL,
    created_by text NOT NULL,
    conversation_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    collaborators jsonb DEFAULT '[]'::jsonb NOT NULL
);

CREATE TABLE public.email_attachments (
    id text NOT NULL,
    message_id text NOT NULL,
    conversation_id text NOT NULL,
    company_id text NOT NULL,
    filename text NOT NULL,
    mime_type text DEFAULT 'application/octet-stream'::text NOT NULL,
    size_bytes bigint DEFAULT 0 NOT NULL,
    storage_key text,
    truncated boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.email_contacts (
    company_id text NOT NULL,
    address text NOT NULL,
    display_name text,
    message_count integer DEFAULT 0 NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.email_messages (
    message_id text NOT NULL,
    conversation_id text NOT NULL,
    company_id text NOT NULL,
    direction text NOT NULL,
    transport_status text NOT NULL,
    transport_error text,
    smtp_message_id text,
    in_reply_to text,
    references_chain jsonb DEFAULT '[]'::jsonb NOT NULL,
    subject text DEFAULT ''::text NOT NULL,
    from_addr text NOT NULL,
    to_addrs jsonb DEFAULT '[]'::jsonb NOT NULL,
    cc_addrs jsonb DEFAULT '[]'::jsonb NOT NULL,
    bcc_addrs jsonb DEFAULT '[]'::jsonb NOT NULL,
    html text,
    raw_size_bytes integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    auto_submitted boolean DEFAULT false NOT NULL,
    retry_attempts integer DEFAULT 0 NOT NULL,
    next_retry_at timestamp with time zone
);

CREATE TABLE public.file_bindings (
    id text NOT NULL,
    company_id text NOT NULL,
    local_root text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.file_snapshots (
    file_id text NOT NULL,
    version bigint NOT NULL,
    storage_key text NOT NULL,
    content_hash text NOT NULL,
    author_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    snapshot_hash text DEFAULT ''::text NOT NULL
);

CREATE TABLE public.files (
    id text NOT NULL,
    binding_id text NOT NULL,
    company_id text NOT NULL,
    rel_path text NOT NULL,
    current_version bigint DEFAULT 0 NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);

CREATE TABLE public.llm_calls (
    id text NOT NULL,
    company_id text,
    agent_id text,
    run_id text,
    conversation_id text,
    purpose text NOT NULL,
    source text DEFAULT 'cloud'::text NOT NULL,
    model text NOT NULL,
    input_tokens integer DEFAULT 0 NOT NULL,
    cached_input_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_tokens integer DEFAULT 0 NOT NULL,
    output_tokens integer DEFAULT 0 NOT NULL,
    reasoning_tokens integer DEFAULT 0 NOT NULL,
    cost_usd double precision DEFAULT 0 NOT NULL,
    cost_estimated boolean DEFAULT true NOT NULL,
    measured boolean DEFAULT true NOT NULL,
    latency_ms integer,
    status text DEFAULT 'ok'::text NOT NULL,
    error text,
    extras jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    daemon_version text
);

CREATE TABLE public.llm_calls_rollup (
    bucket_hour timestamp with time zone NOT NULL,
    company_id text,
    agent_id text,
    purpose text NOT NULL,
    model text NOT NULL,
    source text NOT NULL,
    daemon_version text,
    calls bigint DEFAULT 0 NOT NULL,
    ok_calls bigint DEFAULT 0 NOT NULL,
    failed_calls bigint DEFAULT 0 NOT NULL,
    rate_limited_calls bigint DEFAULT 0 NOT NULL,
    input_tokens bigint DEFAULT 0 NOT NULL,
    cached_input_tokens bigint DEFAULT 0 NOT NULL,
    cache_creation_tokens bigint DEFAULT 0 NOT NULL,
    output_tokens bigint DEFAULT 0 NOT NULL,
    reasoning_tokens bigint DEFAULT 0 NOT NULL,
    cost_usd double precision DEFAULT 0 NOT NULL,
    cost_estimated boolean DEFAULT true NOT NULL
);

CREATE TABLE public.message_reactions (
    message_id text NOT NULL,
    user_id text NOT NULL,
    emoji text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    company_id text
);

CREATE TABLE public.messages (
    id text NOT NULL,
    conversation_id text NOT NULL,
    author_id text NOT NULL,
    kind text NOT NULL,
    body text NOT NULL,
    sequence integer NOT NULL,
    reactions jsonb,
    tool jsonb,
    attachment jsonb,
    client_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    quoted_message_id text,
    company_id text,
    poll jsonb
);

CREATE TABLE public.participants (
    id text NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    role text,
    initial text NOT NULL,
    avatar_bg text NOT NULL,
    status text NOT NULL,
    bio text,
    tools jsonb,
    system_prompt text,
    departed_at timestamp with time zone,
    status_updated_at timestamp with time zone DEFAULT now() NOT NULL,
    avatar_url text,
    model text,
    company_id text NOT NULL,
    email text,
    computer_id text,
    engine text,
    fast_model text
);

CREATE TABLE public.poll_votes (
    message_id text NOT NULL,
    voter_participant_id text NOT NULL,
    voter_kind text NOT NULL,
    option_id text NOT NULL,
    company_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.projects (
    id text NOT NULL,
    company_id text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    color text,
    status text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    archived_at timestamp with time zone
);

CREATE TABLE public.push_devices (
    id text NOT NULL,
    user_id text NOT NULL,
    platform text NOT NULL,
    token text NOT NULL,
    app_version text,
    device_model text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone
);

CREATE TABLE public.sessions (
    token_hash text NOT NULL,
    user_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_used_at timestamp with time zone DEFAULT now() NOT NULL,
    ip text,
    user_agent text
);

CREATE TABLE public.shipping_events (
    id text NOT NULL,
    company_id text NOT NULL,
    feature_id text,
    actor_id text,
    kind text NOT NULL,
    data jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.shipping_features (
    id text NOT NULL,
    company_id text NOT NULL,
    project_id text,
    conversation_id text,
    document_id text,
    board_card_id text,
    title text NOT NULL,
    problem text DEFAULT ''::text NOT NULL,
    desired_outcome text DEFAULT ''::text NOT NULL,
    contract_summary text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'draft'::text NOT NULL,
    priority text DEFAULT 'medium'::text NOT NULL,
    risk_level text DEFAULT 'medium'::text NOT NULL,
    release_target text,
    builder_ids jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_by text NOT NULL,
    updated_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    archived_at timestamp with time zone,
    CONSTRAINT shipping_features_priority_check CHECK ((priority = ANY (ARRAY['critical'::text, 'high'::text, 'medium'::text, 'low'::text]))),
    CONSTRAINT shipping_features_risk_check CHECK ((risk_level = ANY (ARRAY['critical'::text, 'high'::text, 'medium'::text, 'low'::text]))),
    CONSTRAINT shipping_features_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'contract'::text, 'building'::text, 'verifying'::text, 'ready'::text, 'releasing'::text, 'watching'::text, 'learned'::text, 'paused'::text, 'archived'::text])))
);

CREATE TABLE public.shipping_friction_reports (
    id text NOT NULL,
    company_id text NOT NULL,
    feature_id text,
    conversation_id text,
    message_id text,
    reporter_id text,
    source text DEFAULT 'manual'::text NOT NULL,
    title text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    severity text DEFAULT 'medium'::text NOT NULL,
    frequency text DEFAULT 'once'::text NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    evidence jsonb DEFAULT '[]'::jsonb NOT NULL,
    occurrence_count integer DEFAULT 1 NOT NULL,
    first_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    source_key text,
    CONSTRAINT shipping_friction_frequency_check CHECK ((frequency = ANY (ARRAY['once'::text, 'occasional'::text, 'frequent'::text, 'constant'::text]))),
    CONSTRAINT shipping_friction_severity_check CHECK ((severity = ANY (ARRAY['critical'::text, 'high'::text, 'medium'::text, 'low'::text]))),
    CONSTRAINT shipping_friction_status_check CHECK ((status = ANY (ARRAY['open'::text, 'triaged'::text, 'planned'::text, 'resolved'::text, 'dismissed'::text])))
);

CREATE TABLE public.shipping_invariants (
    id text NOT NULL,
    feature_id text NOT NULL,
    title text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    kind text DEFAULT 'behavior'::text NOT NULL,
    required boolean DEFAULT true NOT NULL,
    "position" double precision DEFAULT 0 NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT shipping_invariants_kind_check CHECK ((kind = ANY (ARRAY['behavior'::text, 'architecture'::text, 'data'::text, 'security'::text, 'performance'::text, 'ux'::text, 'operability'::text])))
);

CREATE TABLE public.shipping_regressions (
    id text NOT NULL,
    feature_id text NOT NULL,
    invariant_id text,
    source_verification_id text,
    title text NOT NULL,
    kind text DEFAULT 'automated'::text NOT NULL,
    command text,
    expected text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    last_result text,
    last_evidence jsonb DEFAULT '[]'::jsonb NOT NULL,
    last_run_at timestamp with time zone,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT shipping_regressions_kind_check CHECK ((kind = ANY (ARRAY['automated'::text, 'benchmark'::text, 'manual_replay'::text, 'monitor'::text]))),
    CONSTRAINT shipping_regressions_status_check CHECK ((status = ANY (ARRAY['active'::text, 'passing'::text, 'failing'::text, 'disabled'::text])))
);

CREATE TABLE public.shipping_releases (
    id text NOT NULL,
    feature_id text NOT NULL,
    environment text NOT NULL,
    status text DEFAULT 'planned'::text NOT NULL,
    version text,
    commit_sha text,
    started_by text,
    approved_by text,
    release_notes text DEFAULT ''::text NOT NULL,
    rollback_plan text DEFAULT ''::text NOT NULL,
    known_gaps jsonb DEFAULT '[]'::jsonb NOT NULL,
    baseline jsonb DEFAULT '[]'::jsonb NOT NULL,
    smoke_evidence jsonb DEFAULT '[]'::jsonb NOT NULL,
    readback_due_at timestamp with time zone,
    readback_status text DEFAULT 'pending'::text NOT NULL,
    readback_evidence jsonb DEFAULT '[]'::jsonb NOT NULL,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    rolled_back_at timestamp with time zone,
    rollback_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT shipping_releases_environment_check CHECK ((environment = ANY (ARRAY['development'::text, 'staging'::text, 'canary'::text, 'production'::text]))),
    CONSTRAINT shipping_releases_readback_check CHECK ((readback_status = ANY (ARRAY['pending'::text, 'passed'::text, 'failed'::text, 'overdue'::text]))),
    CONSTRAINT shipping_releases_status_check CHECK ((status = ANY (ARRAY['planned'::text, 'approved'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'rolled_back'::text])))
);

CREATE TABLE public.shipping_verifications (
    id text NOT NULL,
    feature_id text NOT NULL,
    invariant_id text,
    title text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    method text DEFAULT 'user_path'::text NOT NULL,
    required boolean DEFAULT true NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    owner_id text,
    verified_by_id text,
    builder_ids jsonb DEFAULT '[]'::jsonb NOT NULL,
    evidence jsonb DEFAULT '[]'::jsonb NOT NULL,
    notes text DEFAULT ''::text NOT NULL,
    "position" double precision DEFAULT 0 NOT NULL,
    due_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT shipping_verifications_method_check CHECK ((method = ANY (ARRAY['user_path'::text, 'property'::text, 'trace'::text, 'data_reconciliation'::text, 'design_qa'::text, 'security'::text, 'performance'::text, 'release_note'::text]))),
    CONSTRAINT shipping_verifications_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'running'::text, 'passed'::text, 'failed'::text, 'waived'::text]))),
    CONSTRAINT shipping_verifier_not_builder CHECK (((verified_by_id IS NULL) OR (NOT (builder_ids ? verified_by_id))))
);

CREATE TABLE public.tool_calls (
    id text NOT NULL,
    message_id text,
    agent_id text NOT NULL,
    name text NOT NULL,
    args jsonb NOT NULL,
    result jsonb,
    status text DEFAULT 'pending'::text NOT NULL,
    error text,
    duration_ms integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    run_id text,
    company_id text
);

CREATE TABLE public.user_identities (
    provider text NOT NULL,
    provider_id text NOT NULL,
    user_id text NOT NULL,
    email_lower text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.user_preferences (
    user_id text NOT NULL,
    prefs jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    company_id text
);

CREATE TABLE public.users (
    id text NOT NULL,
    email text NOT NULL,
    display_name text NOT NULL,
    password_hash text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_login_at timestamp with time zone,
    email_verified_at timestamp with time zone,
    avatar_url text,
    tier text DEFAULT 'free'::text NOT NULL,
    sub2api_user_id bigint,
    sub2api_api_key text,
    pro_trial_expires_at timestamp with time zone,
    deleted_at timestamp with time zone,
    is_admin boolean DEFAULT false NOT NULL,
    suspended_at timestamp with time zone,
    suspension_reason text,
    suspended_by text
);

CREATE TABLE public.waitlist (
    id text NOT NULL,
    provider text NOT NULL,
    provider_id text NOT NULL,
    email text NOT NULL,
    display_name text NOT NULL,
    avatar_url text,
    status text DEFAULT 'pending'::text NOT NULL,
    note text,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    decided_at timestamp with time zone,
    decided_by text
);

CREATE TABLE public.workspace_associations (
    id text NOT NULL,
    workspace_id text NOT NULL,
    company_id text NOT NULL,
    target_kind text NOT NULL,
    target_id text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workspace_associations_kind_check CHECK ((target_kind = ANY (ARRAY['project'::text, 'board_card'::text, 'document'::text])))
);

CREATE TABLE public.workspace_members (
    workspace_id text NOT NULL,
    participant_id text NOT NULL,
    added_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.workspaces (
    id text NOT NULL,
    company_id text NOT NULL,
    name text NOT NULL,
    folder_path text NOT NULL,
    is_default boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    unbound_at timestamp with time zone,
    unbound_by text
);

CREATE TABLE public.ws_tickets (
    token_hash text NOT NULL,
    user_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone
);

ALTER TABLE ONLY public.audit_events ALTER COLUMN id SET DEFAULT nextval('public.audit_events_id_seq'::regclass);

ALTER TABLE ONLY public.auth_attempts ALTER COLUMN id SET DEFAULT nextval('public.auth_attempts_id_seq'::regclass);

ALTER TABLE ONLY public.document_updates ALTER COLUMN id SET DEFAULT nextval('public.document_updates_id_seq'::regclass);

ALTER TABLE ONLY public.agent_autonomy
    ADD CONSTRAINT agent_autonomy_pkey PRIMARY KEY (user_id, agent_id);

ALTER TABLE ONLY public.agent_climate
    ADD CONSTRAINT agent_climate_pkey PRIMARY KEY (agent_id, about_id);

ALTER TABLE ONLY public.agent_events
    ADD CONSTRAINT agent_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_log
    ADD CONSTRAINT agent_log_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_memory
    ADD CONSTRAINT agent_memory_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_runs
    ADD CONSTRAINT agent_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_tasks
    ADD CONSTRAINT agent_tasks_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_triages
    ADD CONSTRAINT agent_triages_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_workspace
    ADD CONSTRAINT agent_workspace_pkey PRIMARY KEY (agent_id, path);

ALTER TABLE ONLY public.app_settings
    ADD CONSTRAINT app_settings_pkey PRIMARY KEY (key);

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.auth_attempts
    ADD CONSTRAINT auth_attempts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.board_card_comments
    ADD CONSTRAINT board_card_comments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.board_cards
    ADD CONSTRAINT board_cards_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.board_columns
    ADD CONSTRAINT board_columns_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.board_mention_reads
    ADD CONSTRAINT board_mention_reads_pkey PRIMARY KEY (user_id);

ALTER TABLE ONLY public.boards
    ADD CONSTRAINT boards_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.calendar_dispatches
    ADD CONSTRAINT calendar_dispatches_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.calendar_events
    ADD CONSTRAINT calendar_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.calendar_reminders
    ADD CONSTRAINT calendar_reminders_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.companies
    ADD CONSTRAINT companies_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.companies
    ADD CONSTRAINT companies_slug_key UNIQUE (slug);

ALTER TABLE ONLY public.company_invitations
    ADD CONSTRAINT company_invitations_pkey PRIMARY KEY (token_hash);

ALTER TABLE ONLY public.company_members
    ADD CONSTRAINT company_members_pkey PRIMARY KEY (company_id, user_id);

ALTER TABLE ONLY public.computers
    ADD CONSTRAINT computers_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.convene_sessions
    ADD CONSTRAINT convene_sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.convene_transcript
    ADD CONSTRAINT convene_transcript_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.convening_info
    ADD CONSTRAINT convening_info_pkey PRIMARY KEY (conversation_id);

ALTER TABLE ONLY public.conversation_counters
    ADD CONSTRAINT conversation_counters_pkey PRIMARY KEY (conversation_id);

ALTER TABLE ONLY public.conversation_mutes
    ADD CONSTRAINT conversation_mutes_pkey PRIMARY KEY (user_id, conversation_id);

ALTER TABLE ONLY public.conversation_reads
    ADD CONSTRAINT conversation_reads_pkey PRIMARY KEY (user_id, conversation_id);

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.document_mentions
    ADD CONSTRAINT document_mentions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.document_snapshots
    ADD CONSTRAINT document_snapshots_pkey PRIMARY KEY (document_id);

ALTER TABLE ONLY public.document_updates
    ADD CONSTRAINT document_updates_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.documents
    ADD CONSTRAINT documents_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.email_attachments
    ADD CONSTRAINT email_attachments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.email_contacts
    ADD CONSTRAINT email_contacts_pkey PRIMARY KEY (company_id, address);

ALTER TABLE ONLY public.email_messages
    ADD CONSTRAINT email_messages_pkey PRIMARY KEY (message_id);

ALTER TABLE ONLY public.file_bindings
    ADD CONSTRAINT file_bindings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.file_snapshots
    ADD CONSTRAINT file_snapshots_pkey PRIMARY KEY (file_id, version);

ALTER TABLE ONLY public.files
    ADD CONSTRAINT files_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.llm_calls
    ADD CONSTRAINT llm_calls_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.message_reactions
    ADD CONSTRAINT message_reactions_pkey PRIMARY KEY (message_id, user_id, emoji);

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.participants
    ADD CONSTRAINT participants_pkey PRIMARY KEY (id, company_id);

ALTER TABLE ONLY public.poll_votes
    ADD CONSTRAINT poll_votes_pkey PRIMARY KEY (message_id, voter_participant_id, option_id);

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.push_devices
    ADD CONSTRAINT push_devices_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (token_hash);

ALTER TABLE ONLY public.shipping_events
    ADD CONSTRAINT shipping_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.shipping_features
    ADD CONSTRAINT shipping_features_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.shipping_friction_reports
    ADD CONSTRAINT shipping_friction_reports_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.shipping_invariants
    ADD CONSTRAINT shipping_invariants_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.shipping_regressions
    ADD CONSTRAINT shipping_regressions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.shipping_releases
    ADD CONSTRAINT shipping_releases_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.shipping_verifications
    ADD CONSTRAINT shipping_verifications_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.tool_calls
    ADD CONSTRAINT tool_calls_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.user_identities
    ADD CONSTRAINT user_identities_pkey PRIMARY KEY (provider, provider_id);

ALTER TABLE ONLY public.user_preferences
    ADD CONSTRAINT user_preferences_pkey PRIMARY KEY (user_id);

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_email_key UNIQUE (email);

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.waitlist
    ADD CONSTRAINT waitlist_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.waitlist
    ADD CONSTRAINT waitlist_provider_provider_id_key UNIQUE (provider, provider_id);

ALTER TABLE ONLY public.workspace_associations
    ADD CONSTRAINT workspace_associations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.workspace_associations
    ADD CONSTRAINT workspace_associations_unique UNIQUE (workspace_id, target_kind, target_id);

ALTER TABLE ONLY public.workspace_members
    ADD CONSTRAINT workspace_members_pkey PRIMARY KEY (workspace_id, participant_id);

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_folder_unique UNIQUE (folder_path);

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.ws_tickets
    ADD CONSTRAINT ws_tickets_pkey PRIMARY KEY (token_hash);

CREATE UNIQUE INDEX companies_pair_token_idx ON public.companies USING btree (pair_token) WHERE (pair_token IS NOT NULL);

CREATE UNIQUE INDEX computers_pair_token_idx ON public.computers USING btree (pair_token) WHERE (pair_token IS NOT NULL);

CREATE INDEX idx_agent_climate_agent ON public.agent_climate USING btree (agent_id, updated_at DESC);

CREATE INDEX idx_agent_climate_company ON public.agent_climate USING btree (company_id);

CREATE INDEX idx_agent_events_agent ON public.agent_events USING btree (agent_id, created_at DESC);

CREATE INDEX idx_agent_events_company ON public.agent_events USING btree (company_id, created_at DESC);

CREATE INDEX idx_agent_events_created ON public.agent_events USING btree (created_at);

CREATE INDEX idx_agent_events_run ON public.agent_events USING btree (run_id, created_at);

CREATE INDEX idx_agent_log_agent ON public.agent_log USING btree (agent_id, created_at DESC);

CREATE INDEX idx_agent_log_company ON public.agent_log USING btree (company_id, agent_id);

CREATE INDEX idx_agent_log_created ON public.agent_log USING btree (created_at);

CREATE INDEX idx_agent_memory_about ON public.agent_memory USING btree (agent_id, about);

CREATE INDEX idx_agent_memory_agent ON public.agent_memory USING btree (agent_id, created_at DESC);

CREATE INDEX idx_agent_memory_company ON public.agent_memory USING btree (company_id, agent_id);

CREATE INDEX idx_agent_runs_agent_started ON public.agent_runs USING btree (agent_id, started_at DESC);

CREATE INDEX idx_agent_runs_company ON public.agent_runs USING btree (company_id, started_at DESC);

CREATE INDEX idx_agent_runs_company_started ON public.agent_runs USING btree (company_id, started_at DESC);

CREATE INDEX idx_agent_runs_started ON public.agent_runs USING btree (started_at);

CREATE INDEX idx_agent_runs_status_updated ON public.agent_runs USING btree (status, updated_at DESC);

CREATE INDEX idx_agent_tasks_agent ON public.agent_tasks USING btree (agent_id, status, updated_at DESC);

CREATE INDEX idx_agent_triages_agent_created ON public.agent_triages USING btree (agent_id, created_at DESC);

CREATE INDEX idx_agent_triages_company_created ON public.agent_triages USING btree (company_id, created_at DESC);

CREATE INDEX idx_agent_workspace_agent ON public.agent_workspace USING btree (agent_id, updated_at DESC);

CREATE INDEX idx_audit_events_kind ON public.audit_events USING btree (kind, created_at DESC);

CREATE INDEX idx_audit_events_user ON public.audit_events USING btree (user_id, created_at DESC);

CREATE INDEX idx_auth_attempts_email ON public.auth_attempts USING btree (email, created_at DESC);

CREATE INDEX idx_auth_attempts_ip ON public.auth_attempts USING btree (ip, created_at DESC);

CREATE INDEX idx_board_card_comments_card ON public.board_card_comments USING btree (card_id, created_at);

CREATE INDEX idx_board_cards_assignee ON public.board_cards USING btree (assignee_id) WHERE (assignee_id IS NOT NULL);

CREATE INDEX idx_board_cards_board ON public.board_cards USING btree (board_id, updated_at DESC);

CREATE INDEX idx_board_cards_column ON public.board_cards USING btree (column_id, "position");

CREATE INDEX idx_board_columns_board ON public.board_columns USING btree (board_id, "position");

CREATE INDEX idx_boards_company ON public.boards USING btree (company_id, updated_at DESC);

CREATE INDEX idx_calendar_dispatches_company ON public.calendar_dispatches USING btree (company_id, dispatched_at DESC);

CREATE INDEX idx_calendar_dispatches_event ON public.calendar_dispatches USING btree (event_id, scheduled_for DESC);

CREATE INDEX idx_calendar_events_assignee ON public.calendar_events USING btree (assignee_id, start_at) WHERE (assignee_id IS NOT NULL);

CREATE INDEX idx_calendar_events_company_start ON public.calendar_events USING btree (company_id, start_at);

CREATE INDEX idx_calendar_events_status ON public.calendar_events USING btree (status, start_at) WHERE (status = 'active'::text);

CREATE INDEX idx_calendar_reminders_event ON public.calendar_reminders USING btree (event_id, scheduled_for DESC);

CREATE INDEX idx_company_invitations_company ON public.company_invitations USING btree (company_id, created_at DESC);

CREATE INDEX idx_company_invitations_email ON public.company_invitations USING btree (email) WHERE (email IS NOT NULL);

CREATE INDEX idx_computers_company ON public.computers USING btree (company_id);

CREATE INDEX idx_convene_transcript_session_seq ON public.convene_transcript USING btree (session_id, sequence);

CREATE INDEX idx_conversations_company ON public.conversations USING btree (company_id, updated_at DESC);

CREATE INDEX idx_conversations_members_gin ON public.conversations USING gin (members jsonb_path_ops);

CREATE INDEX idx_conversations_project ON public.conversations USING btree (project_id);

CREATE INDEX idx_document_mentions_doc ON public.document_mentions USING btree (document_id, created_at DESC);

CREATE INDEX idx_document_mentions_recipient ON public.document_mentions USING btree (mentioned_id, created_at DESC);

CREATE INDEX idx_document_updates_doc ON public.document_updates USING btree (document_id, id);

CREATE INDEX idx_documents_company_updated ON public.documents USING btree (company_id, updated_at DESC);

CREATE INDEX idx_documents_conversation ON public.documents USING btree (conversation_id) WHERE (conversation_id IS NOT NULL);

CREATE INDEX idx_email_attachments_conv ON public.email_attachments USING btree (conversation_id, created_at DESC);

CREATE INDEX idx_email_attachments_msg ON public.email_attachments USING btree (message_id);

CREATE INDEX idx_email_contacts_seen ON public.email_contacts USING btree (company_id, last_seen_at DESC);

CREATE INDEX idx_email_messages_company ON public.email_messages USING btree (company_id, created_at DESC);

CREATE INDEX idx_email_messages_conv ON public.email_messages USING btree (conversation_id, created_at DESC);

CREATE INDEX idx_email_messages_retry_due ON public.email_messages USING btree (next_retry_at) WHERE ((direction = 'out'::text) AND (transport_status = 'failed'::text) AND (next_retry_at IS NOT NULL));

CREATE UNIQUE INDEX idx_file_bindings_company ON public.file_bindings USING btree (company_id);

CREATE INDEX idx_file_snapshots_file ON public.file_snapshots USING btree (file_id, version DESC);

CREATE UNIQUE INDEX idx_files_binding_path ON public.files USING btree (binding_id, rel_path);

CREATE INDEX idx_files_company ON public.files USING btree (company_id);

CREATE INDEX idx_llm_calls_agent_created ON public.llm_calls USING btree (agent_id, created_at DESC) WHERE (agent_id IS NOT NULL);

CREATE INDEX idx_llm_calls_company_purpose_created ON public.llm_calls USING btree (company_id, purpose, created_at DESC);

CREATE INDEX idx_llm_calls_created ON public.llm_calls USING btree (created_at);

CREATE INDEX idx_llm_calls_created_brin ON public.llm_calls USING brin (created_at);

CREATE INDEX idx_llm_calls_daemon_version_created ON public.llm_calls USING btree (daemon_version, created_at DESC) WHERE (daemon_version IS NOT NULL);

CREATE INDEX idx_llm_calls_model_purpose_created ON public.llm_calls USING btree (model, purpose, created_at DESC);

CREATE INDEX idx_llm_calls_run_created ON public.llm_calls USING btree (run_id, created_at) WHERE (run_id IS NOT NULL);

CREATE INDEX idx_llm_rollup_bucket ON public.llm_calls_rollup USING btree (bucket_hour DESC);

CREATE UNIQUE INDEX idx_llm_rollup_key ON public.llm_calls_rollup USING btree (bucket_hour, company_id, agent_id, purpose, model, source, daemon_version) NULLS NOT DISTINCT;

CREATE INDEX idx_messages_author_created ON public.messages USING btree (author_id, created_at DESC);

CREATE INDEX idx_messages_company ON public.messages USING btree (company_id, created_at DESC);

CREATE INDEX idx_messages_convo_created ON public.messages USING btree (conversation_id, created_at);

CREATE INDEX idx_messages_convo_seq ON public.messages USING btree (conversation_id, sequence);

CREATE INDEX idx_messages_quoted ON public.messages USING btree (quoted_message_id);

CREATE INDEX idx_participants_active ON public.participants USING btree (kind) WHERE (departed_at IS NULL);

CREATE INDEX idx_participants_company ON public.participants USING btree (company_id);

CREATE INDEX idx_participants_email_lower ON public.participants USING btree (lower(email)) WHERE (email IS NOT NULL);

CREATE INDEX idx_poll_votes_message ON public.poll_votes USING btree (message_id);

CREATE INDEX idx_poll_votes_voter ON public.poll_votes USING btree (voter_participant_id);

CREATE INDEX idx_projects_company ON public.projects USING btree (company_id, status);

CREATE UNIQUE INDEX idx_push_devices_platform_token ON public.push_devices USING btree (platform, token);

CREATE INDEX idx_push_devices_user ON public.push_devices USING btree (user_id) WHERE (disabled_at IS NULL);

CREATE INDEX idx_sessions_expires ON public.sessions USING btree (expires_at);

CREATE INDEX idx_sessions_user ON public.sessions USING btree (user_id);

CREATE INDEX idx_shipping_events_feature ON public.shipping_events USING btree (feature_id, created_at);

CREATE INDEX idx_shipping_features_company_status ON public.shipping_features USING btree (company_id, status, updated_at DESC);

CREATE INDEX idx_shipping_features_project ON public.shipping_features USING btree (project_id, updated_at DESC) WHERE (project_id IS NOT NULL);

CREATE INDEX idx_shipping_friction_company ON public.shipping_friction_reports USING btree (company_id, status, severity, last_seen_at DESC);

CREATE UNIQUE INDEX idx_shipping_friction_source_key ON public.shipping_friction_reports USING btree (company_id, source_key) WHERE (source_key IS NOT NULL);

CREATE INDEX idx_shipping_invariants_feature ON public.shipping_invariants USING btree (feature_id, "position");

CREATE INDEX idx_shipping_regressions_feature ON public.shipping_regressions USING btree (feature_id, status, updated_at DESC);

CREATE UNIQUE INDEX idx_shipping_regressions_source_verification ON public.shipping_regressions USING btree (source_verification_id) WHERE (source_verification_id IS NOT NULL);

CREATE INDEX idx_shipping_releases_feature ON public.shipping_releases USING btree (feature_id, created_at DESC);

CREATE INDEX idx_shipping_releases_readback ON public.shipping_releases USING btree (readback_status, readback_due_at) WHERE ((status = 'succeeded'::text) AND (environment = 'production'::text));

CREATE INDEX idx_shipping_verifications_feature ON public.shipping_verifications USING btree (feature_id, "position");

CREATE INDEX idx_shipping_verifications_owner ON public.shipping_verifications USING btree (owner_id, status, updated_at DESC) WHERE (owner_id IS NOT NULL);

CREATE INDEX idx_tool_calls_agent ON public.tool_calls USING btree (agent_id, created_at DESC);

CREATE INDEX idx_tool_calls_run ON public.tool_calls USING btree (run_id);

CREATE INDEX idx_user_identities_email ON public.user_identities USING btree (email_lower);

CREATE INDEX idx_user_identities_user ON public.user_identities USING btree (user_id);

CREATE INDEX idx_users_deleted_at ON public.users USING btree (deleted_at) WHERE (deleted_at IS NOT NULL);

CREATE INDEX idx_users_email ON public.users USING btree (lower(email));

CREATE INDEX idx_users_is_admin ON public.users USING btree (is_admin) WHERE (is_admin = true);

CREATE INDEX idx_users_suspended ON public.users USING btree (suspended_at) WHERE (suspended_at IS NOT NULL);

CREATE INDEX idx_waitlist_email ON public.waitlist USING btree (lower(email));

CREATE INDEX idx_waitlist_status ON public.waitlist USING btree (status, requested_at);

CREATE INDEX idx_workspace_associations_target ON public.workspace_associations USING btree (target_kind, target_id);

CREATE INDEX idx_workspace_associations_workspace ON public.workspace_associations USING btree (workspace_id);

CREATE INDEX idx_workspace_embed_hnsw ON public.agent_workspace USING hnsw (embedding public.vector_cosine_ops) WHERE (path ~~ 'memory/%'::text);

CREATE INDEX idx_workspace_members_participant ON public.workspace_members USING btree (participant_id);

CREATE INDEX idx_workspace_memory_about ON public.agent_workspace USING btree (((meta ->> 'about'::text))) WHERE (path ~~ 'memory/%'::text);

CREATE INDEX idx_workspaces_company ON public.workspaces USING btree (company_id, created_at);

CREATE UNIQUE INDEX idx_workspaces_default_one_per_company ON public.workspaces USING btree (company_id) WHERE is_default;

CREATE INDEX idx_ws_tickets_expires ON public.ws_tickets USING btree (expires_at);

CREATE INDEX idx_ws_tickets_user ON public.ws_tickets USING btree (user_id);

CREATE UNIQUE INDEX participants_agent_id_unique ON public.participants USING btree (id) WHERE (kind = 'agent'::text);

CREATE UNIQUE INDEX uniq_calendar_dispatch_slot ON public.calendar_dispatches USING btree (event_id, scheduled_for);

CREATE UNIQUE INDEX uniq_calendar_reminders_slot ON public.calendar_reminders USING btree (event_id, scheduled_for);

CREATE UNIQUE INDEX uniq_email_messages_smtp_id ON public.email_messages USING btree (lower(smtp_message_id)) WHERE (smtp_message_id IS NOT NULL);

CREATE UNIQUE INDEX uniq_messages_client_id ON public.messages USING btree (conversation_id, author_id, client_id) WHERE (client_id IS NOT NULL);

CREATE UNIQUE INDEX uniq_participants_email ON public.participants USING btree (lower(email)) WHERE (email IS NOT NULL);

ALTER TABLE ONLY public.agent_events
    ADD CONSTRAINT agent_events_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.agent_runs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.board_card_comments
    ADD CONSTRAINT board_card_comments_card_id_fkey FOREIGN KEY (card_id) REFERENCES public.board_cards(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.board_cards
    ADD CONSTRAINT board_cards_board_id_fkey FOREIGN KEY (board_id) REFERENCES public.boards(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.board_cards
    ADD CONSTRAINT board_cards_column_id_fkey FOREIGN KEY (column_id) REFERENCES public.board_columns(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.board_columns
    ADD CONSTRAINT board_columns_board_id_fkey FOREIGN KEY (board_id) REFERENCES public.boards(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.boards
    ADD CONSTRAINT boards_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.calendar_dispatches
    ADD CONSTRAINT calendar_dispatches_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.calendar_events(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.calendar_events
    ADD CONSTRAINT calendar_events_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.calendar_events
    ADD CONSTRAINT calendar_events_target_conversation_id_fkey FOREIGN KEY (target_conversation_id) REFERENCES public.conversations(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.calendar_reminders
    ADD CONSTRAINT calendar_reminders_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.calendar_events(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.companies
    ADD CONSTRAINT companies_all_hands_conversation_id_fkey FOREIGN KEY (all_hands_conversation_id) REFERENCES public.conversations(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.company_invitations
    ADD CONSTRAINT company_invitations_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.company_members
    ADD CONSTRAINT company_members_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.convene_sessions
    ADD CONSTRAINT convene_sessions_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.convene_transcript
    ADD CONSTRAINT convene_transcript_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.convene_sessions(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.convening_info
    ADD CONSTRAINT convening_info_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.conversation_counters
    ADD CONSTRAINT conversation_counters_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.conversation_mutes
    ADD CONSTRAINT conversation_mutes_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.conversation_reads
    ADD CONSTRAINT conversation_reads_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.document_mentions
    ADD CONSTRAINT document_mentions_document_id_fkey FOREIGN KEY (document_id) REFERENCES public.documents(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.document_snapshots
    ADD CONSTRAINT document_snapshots_document_id_fkey FOREIGN KEY (document_id) REFERENCES public.documents(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.document_updates
    ADD CONSTRAINT document_updates_document_id_fkey FOREIGN KEY (document_id) REFERENCES public.documents(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.documents
    ADD CONSTRAINT documents_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.email_attachments
    ADD CONSTRAINT email_attachments_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.email_attachments
    ADD CONSTRAINT email_attachments_message_id_fkey FOREIGN KEY (message_id) REFERENCES public.messages(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.email_messages
    ADD CONSTRAINT email_messages_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.email_messages
    ADD CONSTRAINT email_messages_message_id_fkey FOREIGN KEY (message_id) REFERENCES public.messages(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.file_snapshots
    ADD CONSTRAINT file_snapshots_file_id_fkey FOREIGN KEY (file_id) REFERENCES public.files(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.files
    ADD CONSTRAINT files_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.file_bindings(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.message_reactions
    ADD CONSTRAINT message_reactions_message_id_fkey FOREIGN KEY (message_id) REFERENCES public.messages(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.poll_votes
    ADD CONSTRAINT poll_votes_message_id_fkey FOREIGN KEY (message_id) REFERENCES public.messages(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.push_devices
    ADD CONSTRAINT push_devices_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_events
    ADD CONSTRAINT shipping_events_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_events
    ADD CONSTRAINT shipping_events_feature_id_fkey FOREIGN KEY (feature_id) REFERENCES public.shipping_features(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_features
    ADD CONSTRAINT shipping_features_board_card_id_fkey FOREIGN KEY (board_card_id) REFERENCES public.board_cards(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.shipping_features
    ADD CONSTRAINT shipping_features_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_features
    ADD CONSTRAINT shipping_features_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.shipping_features
    ADD CONSTRAINT shipping_features_document_id_fkey FOREIGN KEY (document_id) REFERENCES public.documents(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.shipping_features
    ADD CONSTRAINT shipping_features_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.shipping_friction_reports
    ADD CONSTRAINT shipping_friction_reports_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_friction_reports
    ADD CONSTRAINT shipping_friction_reports_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.shipping_friction_reports
    ADD CONSTRAINT shipping_friction_reports_feature_id_fkey FOREIGN KEY (feature_id) REFERENCES public.shipping_features(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.shipping_invariants
    ADD CONSTRAINT shipping_invariants_feature_id_fkey FOREIGN KEY (feature_id) REFERENCES public.shipping_features(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_regressions
    ADD CONSTRAINT shipping_regressions_feature_id_fkey FOREIGN KEY (feature_id) REFERENCES public.shipping_features(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_regressions
    ADD CONSTRAINT shipping_regressions_invariant_id_fkey FOREIGN KEY (invariant_id) REFERENCES public.shipping_invariants(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.shipping_regressions
    ADD CONSTRAINT shipping_regressions_source_verification_id_fkey FOREIGN KEY (source_verification_id) REFERENCES public.shipping_verifications(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.shipping_releases
    ADD CONSTRAINT shipping_releases_feature_id_fkey FOREIGN KEY (feature_id) REFERENCES public.shipping_features(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_verifications
    ADD CONSTRAINT shipping_verifications_feature_id_fkey FOREIGN KEY (feature_id) REFERENCES public.shipping_features(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.shipping_verifications
    ADD CONSTRAINT shipping_verifications_invariant_id_fkey FOREIGN KEY (invariant_id) REFERENCES public.shipping_invariants(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.tool_calls
    ADD CONSTRAINT tool_calls_message_id_fkey FOREIGN KEY (message_id) REFERENCES public.messages(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.user_identities
    ADD CONSTRAINT user_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.workspace_associations
    ADD CONSTRAINT workspace_associations_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.workspace_associations
    ADD CONSTRAINT workspace_associations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.workspace_members
    ADD CONSTRAINT workspace_members_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_company_id_fkey FOREIGN KEY (company_id) REFERENCES public.companies(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.ws_tickets
    ADD CONSTRAINT ws_tickets_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
