-- RE · categories allocated at runtime by parameter maintenance (T24: CATEGORY). Built-in
-- categories stay in code; these extend them (suspense types, for example).
CREATE TABLE categories (
    id         TEXT PRIMARY KEY,
    code       INTEGER NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    gl_code    TEXT NOT NULL,
    owner      TEXT NOT NULL,
    created_at DATETIME NOT NULL
);
