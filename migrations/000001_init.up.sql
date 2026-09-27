-- Схема данных
-- chat_id и user_id — идентификаторы MAX (bigint, chat_id может быть отрицательным).

CREATE TABLE house_chats (
    id         bigint PRIMARY KEY,
    title      text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'active',  -- pending, active, bot_removed
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id             bigint PRIMARY KEY,
    nickname       text NOT NULL DEFAULT '',
    -- Бот не может писать пользователю в личку (не начинал диалог или
    -- заблокировал бота). Сбрасывается, когда пользователь пишет боту.
    dm_unavailable bool NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    chat_id          bigint NOT NULL REFERENCES house_chats (id),
    user_id          bigint NOT NULL REFERENCES users (id),
    role             text NOT NULL DEFAULT 'member',
    rating           int NOT NULL DEFAULT 0,
    forward_to_dm    bool NOT NULL DEFAULT false,  -- новые заявки дома в личку
    notify_materials bool NOT NULL DEFAULT true,   -- уведомления о материалах к своим заявкам
    status           text NOT NULL DEFAULT 'active',
    joined_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, user_id)
);

CREATE INDEX memberships_user_idx ON memberships (user_id);
-- Новые заявки в личку (раздел 8.6).
CREATE INDEX memberships_forward_idx ON memberships (chat_id) WHERE forward_to_dm;

CREATE TABLE requests (
    id              bigserial PRIMARY KEY,
    chat_id         bigint NOT NULL REFERENCES house_chats (id),
    author_id       bigint NOT NULL REFERENCES users (id),
    type            text NOT NULL,
    status          text NOT NULL DEFAULT 'open',
    is_anonymous    bool NOT NULL DEFAULT false,
    body            text NOT NULL,
    result_text     text,
    rating_applied  bool NOT NULL DEFAULT false,
    chat_message_id text,
    chat_sync       text NOT NULL DEFAULT 'ok',
    -- Окончание голосования храним явно: VOTING_DURATION может
    -- переопределяться по типу заявки (раздел 8.0).
    voting_ends_at  timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    closed_at       timestamptz
);

-- Суточный лимит: заявки автора в чате за скользящие 24 часа.
CREATE INDEX requests_chat_author_created_idx ON requests (chat_id, author_id, created_at);
-- Воркер планировщика: open/in_progress с истёкшим expires_at.
CREATE INDEX requests_status_expires_idx ON requests (status, expires_at);
-- Воркер планировщика: open с завершённым голосованием.
CREATE INDEX requests_status_voting_ends_idx ON requests (status, voting_ends_at);

CREATE TABLE comments (
    id         bigserial PRIMARY KEY,
    request_id bigint NOT NULL REFERENCES requests (id),
    author_id  bigint NOT NULL REFERENCES users (id),
    kind       text NOT NULL,
    body       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX comments_request_idx ON comments (request_id, created_at);

CREATE TABLE attachments (
    id         bigserial PRIMARY KEY,
    request_id bigint REFERENCES requests (id),
    comment_id bigint REFERENCES comments (id),
    kind       text NOT NULL,
    max_ref    text NOT NULL,
    CHECK (request_id IS NOT NULL OR comment_id IS NOT NULL)
);

CREATE INDEX attachments_request_idx ON attachments (request_id);
CREATE INDEX attachments_comment_idx ON attachments (comment_id);

CREATE TABLE votes (
    request_id bigint NOT NULL REFERENCES requests (id),
    user_id    bigint NOT NULL REFERENCES users (id),
    value      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id, user_id)
);

CREATE INDEX votes_user_idx ON votes (user_id);

CREATE TABLE rating_events (
    id         bigserial PRIMARY KEY,
    chat_id    bigint NOT NULL REFERENCES house_chats (id),
    user_id    bigint NOT NULL REFERENCES users (id),
    request_id bigint REFERENCES requests (id),
    delta      int NOT NULL,
    reason     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX rating_events_membership_idx ON rating_events (chat_id, user_id);

-- Последний запуск периодических задач планировщика (сброс рейтингов):
-- строка блокируется FOR UPDATE, поэтому при нескольких репликах задача
-- выполняется один раз за период.
CREATE TABLE scheduler_runs (
    job         text PRIMARY KEY,
    last_run_at timestamptz NOT NULL
);

-- Факт рассылки по заявке (раздел 8.5): текст не храним, только аудиторию
-- и число получателей.
CREATE TABLE broadcasts (
    id         bigserial PRIMARY KEY,
    request_id bigint NOT NULL REFERENCES requests (id),
    audience   text NOT NULL,
    recipients int NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX broadcasts_request_idx ON broadcasts (request_id, created_at DESC);

-- Сообщения о бытовых проблемах (раздел 8.8). Строка problem_reports —
-- «инцидент»: проблема в доме в пределах окна PROBLEM_ALERT_WINDOW с первого
-- сообщения. Кто и когда сообщил — в problem_report_users (житель в окне
-- один раз); при PROBLEM_ALERT_THRESHOLD жителях в чат уходит оповещение
-- с упоминанием админов.
CREATE TABLE problem_reports (
    id                bigserial PRIMARY KEY,
    chat_id           bigint NOT NULL REFERENCES house_chats (id),
    problem           text NOT NULL,          -- код: no_internet, no_power, ...
    first_reported_at timestamptz NOT NULL,   -- начало окна
    notified          bool NOT NULL DEFAULT false,
    notified_at       timestamptz
);

-- Поиск открытого окна по проблеме в доме.
CREATE INDEX problem_reports_open_idx ON problem_reports (chat_id, problem, first_reported_at);

-- Жители, сообщившие о проблеме в окне. Первичный ключ не даёт посчитать
-- жителя дважды и служит индексом для подсчёта по окну.
CREATE TABLE problem_report_users (
    report_id   bigint NOT NULL REFERENCES problem_reports (id) ON DELETE CASCADE,
    user_id     bigint NOT NULL REFERENCES users (id),
    reported_at timestamptz NOT NULL,
    PRIMARY KEY (report_id, user_id)
);
