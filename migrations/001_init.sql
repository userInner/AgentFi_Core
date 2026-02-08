-- 001_init.sql: Initial schema for AgentFi Go Backend
-- Requirements: 12.1 (core tables), 12.3 (indexes), 12.4 (UUID primary keys)

-- Enable UUID generation
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Users
CREATE TABLE users (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    address    VARCHAR(42) UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- MCP Servers
CREATE TABLE mcp_servers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       VARCHAR(100) NOT NULL,
    url        TEXT NOT NULL,
    type       VARCHAR(20) NOT NULL DEFAULT 'http',
    tools      JSONB NOT NULL DEFAULT '[]',
    is_public  BOOLEAN NOT NULL DEFAULT false,
    owner_id   UUID REFERENCES users(id),
    status     VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Agents
CREATE TABLE agents (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id),
    name            VARCHAR(100) NOT NULL,
    strategy_prompt TEXT NOT NULL,
    pair            VARCHAR(20) NOT NULL,
    interval_sec    INT NOT NULL DEFAULT 60,
    status          VARCHAR(20) NOT NULL DEFAULT 'created',
    risk_params     JSONB NOT NULL,
    llm_params      JSONB NOT NULL DEFAULT '{}',
    state           JSONB NOT NULL DEFAULT '{}',
    aum             BIGINT NOT NULL DEFAULT 0,
    investor_count  INT NOT NULL DEFAULT 0,
    total_pnl       BIGINT NOT NULL DEFAULT 0,
    trade_count     INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Agent ↔ MCP Server bindings
CREATE TABLE agent_mcp_bindings (
    agent_id  UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    server_id UUID NOT NULL REFERENCES mcp_servers(id),
    PRIMARY KEY (agent_id, server_id)
);

-- Investments
CREATE TABLE investments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id),
    agent_id    UUID NOT NULL REFERENCES agents(id),
    amount      BIGINT NOT NULL,
    share_ratio NUMERIC(18,8) NOT NULL,
    status      VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    redeemed_at TIMESTAMPTZ
);

-- Trade logs
CREATE TABLE trade_logs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id      UUID NOT NULL REFERENCES agents(id),
    cycle_num     BIGINT NOT NULL,
    action        VARCHAR(10) NOT NULL,
    pair          VARCHAR(20) NOT NULL,
    amount        BIGINT NOT NULL,
    price         BIGINT NOT NULL,
    pnl           BIGINT NOT NULL DEFAULT 0,
    risk_approved BOOLEAN NOT NULL,
    risk_reason   TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Agent execution cycle logs
CREATE TABLE agent_cycles (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id      UUID NOT NULL REFERENCES agents(id),
    cycle_num     BIGINT NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ,
    duration_ms   INT,
    llm_prompt    TEXT,
    llm_response  TEXT,
    tool_calls    JSONB DEFAULT '[]',
    token_usage   JSONB,
    fast_path_hit BOOLEAN NOT NULL DEFAULT false,
    error         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Risk events
CREATE TABLE risk_events (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id   UUID NOT NULL REFERENCES agents(id),
    event_type VARCHAR(50) NOT NULL,
    details    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Ledger entries (fund flows)
CREATE TABLE ledger_entries (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id),
    agent_id   UUID NOT NULL REFERENCES agents(id),
    type       VARCHAR(20) NOT NULL,
    amount     BIGINT NOT NULL,
    balance    BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes (Requirement 12.3)
CREATE INDEX idx_agents_user_id ON agents(user_id);
CREATE INDEX idx_agents_status ON agents(status);
CREATE INDEX idx_investments_user_id ON investments(user_id);
CREATE INDEX idx_investments_agent_id ON investments(agent_id);
CREATE INDEX idx_trade_logs_agent_id ON trade_logs(agent_id);
CREATE INDEX idx_trade_logs_created_at ON trade_logs(created_at);
CREATE INDEX idx_agent_cycles_agent_id ON agent_cycles(agent_id);
CREATE INDEX idx_ledger_entries_user_id ON ledger_entries(user_id);
CREATE INDEX idx_ledger_entries_agent_id ON ledger_entries(agent_id);
CREATE INDEX idx_risk_events_agent_id ON risk_events(agent_id);
