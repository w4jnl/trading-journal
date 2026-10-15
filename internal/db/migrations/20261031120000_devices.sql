-- migrate:up
-- Phones registered for push (the companion app). A device is one APNs
-- token: sandbox says which Apple service it belongs to (an Xcode build
-- versus TestFlight), prefs is the app's notification settings as JSON
-- (the server filters, iOS cannot drop a push once it arrived),
-- activity_start_token is the push-to-start token for the Session Live
-- Activity. device_activities are the running activities with their update
-- tokens; device_sessions remembers the days a session was started on a
-- device, once per day.
CREATE TABLE devices (
    id                   INTEGER PRIMARY KEY,
    user_id              INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token                TEXT NOT NULL UNIQUE,
    name                 TEXT NOT NULL DEFAULT '',
    sandbox              INTEGER NOT NULL DEFAULT 0,
    bundle_id            TEXT NOT NULL DEFAULT '',
    prefs                TEXT NOT NULL DEFAULT '{}',
    activity_start_token TEXT,
    created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_devices_user ON devices(user_id);

CREATE TABLE device_activities (
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    activity_id TEXT NOT NULL,
    token       TEXT NOT NULL,
    kind        TEXT NOT NULL DEFAULT 'session',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (device_id, activity_id)
);

CREATE TABLE device_sessions (
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    day        TEXT NOT NULL,
    started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (device_id, day)
);

-- migrate:down
DROP TABLE device_sessions;
DROP TABLE device_activities;
DROP INDEX idx_devices_user;
DROP TABLE devices;
