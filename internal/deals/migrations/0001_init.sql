CREATE TABLE deals.deals (
    id                uuid PRIMARY KEY,
    code              text        NOT NULL UNIQUE CHECK (char_length(code) <= 20),
    name              text        NOT NULL,
    aisle            text        NOT NULL,
    brand            text        NOT NULL,
    sale_date      timestamptz NOT NULL,
    price_cents       bigint      NOT NULL,
    pieces_total       integer     NOT NULL,
    pieces_left        integer     NOT NULL,
    max_per_shopper integer     NOT NULL,
    created_at        timestamptz NOT NULL,
    CONSTRAINT deals_remaining CHECK (pieces_left >= 0 AND pieces_left <= pieces_total),
    CONSTRAINT deals_cap CHECK (max_per_shopper >= 1 AND max_per_shopper <= pieces_total)
);

-- How many pieces one shopper holds in one deal. Written only by the reserve and release handlers.
CREATE TABLE deals.shoppers (
    deal_id  uuid    NOT NULL REFERENCES deals.deals (id),
    owner_id uuid    NOT NULL,
    pieces    integer NOT NULL CHECK (pieces >= 0),
    PRIMARY KEY (deal_id, owner_id)
);

CREATE TABLE deals.piece_holds (
    order_id       uuid PRIMARY KEY,
    owner_id       uuid        NOT NULL,
    deal_id        uuid        NOT NULL REFERENCES deals.deals (id),
    pieces          integer     NOT NULL,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text,
    -- a pending hold keeps its pieces until expires_at unless the shopper confirms it first
    expires_at     timestamptz,
    reference      text        NOT NULL DEFAULT '',
    confirmed      boolean     NOT NULL DEFAULT true
);
CREATE INDEX piece_holds_expiry ON deals.piece_holds (expires_at) WHERE status = 'Held' AND NOT confirmed;

CREATE TABLE deals.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX deals_outbox_unsent ON deals.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE deals.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);
