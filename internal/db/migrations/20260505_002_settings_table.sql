-- Singleton table holding the user's settings, formerly serialized to
-- ~/.triagefactory/config.yaml. The CHECK (id = 1) keeps it strictly
-- one row — Save() upserts against id=1, Load() reads it. The blob is
-- still YAML so anyone reaching for `sqlite3` to inspect gets a
-- readable dump, and the legacy-YAML import stores a re-marshaled YAML
-- rendering of the config after forcing the new poll-interval default.
--
-- The actual import of an existing config.yaml runs in Go via
-- config.MigrateLegacyYAML, which entrypoints call after config.Init
-- (it needs to read the file, mutate the struct, and delete the YAML
-- on success — none of which is expressible in pure SQL). This file
-- just creates the table; the row is populated either by that import
-- or, on fresh installs, by the first Save() from the Settings UI.
CREATE TABLE IF NOT EXISTS settings (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    data       TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
