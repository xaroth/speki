package index

// schemaVersion changes whenever schema does. An index with another version is rebuilt; there are no
// migrations, because rebuilding from git is always possible.
const schemaVersion = "3"

// schema creates one vault's index, without the triggers. Documents are only ever inserted and deleted,
// never updated.
const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;

CREATE TABLE documents (
	id          INTEGER PRIMARY KEY,
	path        TEXT NOT NULL UNIQUE, -- exactly as in git
	key         TEXT NOT NULL,        -- path in NFC, which links are matched against
	name        TEXT NOT NULL,        -- lowercased file name without .md, in NFC, for wikilinks
	title       TEXT NOT NULL,
	description TEXT,
	status      TEXT NOT NULL,        -- draft | active | deprecated
	tags        TEXT NOT NULL,        -- JSON array
	aliases     TEXT NOT NULL,        -- JSON array
	related     TEXT NOT NULL,        -- JSON array
	created     TEXT,
	updated     TEXT,
	extra       TEXT NOT NULL,        -- JSON object of unknown frontmatter keys
	headings    TEXT NOT NULL,        -- heading texts joined by newlines, for full-text search
	body        TEXT NOT NULL,        -- markdown without frontmatter
	bytes       INTEGER NOT NULL,
	lines       INTEGER NOT NULL,
	version     TEXT NOT NULL,        -- git blob SHA
	warnings    TEXT NOT NULL         -- JSON array of frontmatter problems
);
CREATE INDEX documents_key ON documents (key);
CREATE INDEX documents_name ON documents (name);
CREATE INDEX documents_updated ON documents (updated);
CREATE INDEX documents_deprecated ON documents (id) WHERE status = 'deprecated'; -- for ranking

CREATE TABLE tags (
	doc_id INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
	tag    TEXT NOT NULL,
	PRIMARY KEY (doc_id, tag)
) WITHOUT ROWID;
CREATE INDEX tags_tag ON tags (tag);

CREATE TABLE links (
	doc_id    INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
	target    TEXT NOT NULL,
	candidate TEXT,          -- in NFC, like documents.key and documents.name
	kind      TEXT NOT NULL,
	text      TEXT
);
CREATE INDEX links_doc ON links (doc_id);
CREATE INDEX links_candidate ON links (candidate);

CREATE TABLE headings (
	doc_id INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
	level  INTEGER NOT NULL,
	text   TEXT NOT NULL,
	line   INTEGER NOT NULL
);
CREATE INDEX headings_doc ON headings (doc_id);

-- Ranked search, stemmed. Column order matches the bm25 weights in search.go.
CREATE VIRTUAL TABLE fts USING fts5 (
	title, aliases, tags, description, headings, body,
	content = 'documents', content_rowid = 'id',
	tokenize = 'porter unicode61 remove_diacritics 2',
	prefix = '2 3'
);

-- Substring matches on names: titles, aliases and file names (not folders, or "ops" would match
-- everything under ops/).
CREATE VIRTUAL TABLE names USING fts5 (
	title, aliases, name,
	content = 'documents', content_rowid = 'id',
	tokenize = 'trigram'
);

-- Unstemmed vocabulary for typo correction. No positions, so it stays small.
CREATE VIRTUAL TABLE words USING fts5 (
	title, aliases, tags, description, headings, body,
	content = 'documents', content_rowid = 'id',
	tokenize = 'unicode61 remove_diacritics 2',
	detail = none
);
CREATE VIRTUAL TABLE words_vocab USING fts5vocab (words, 'row');

-- Links resolved when read, so adding a document fixes the links pointing at it. The shortest path wins
-- for ambiguous wikilinks, close to what Obsidian does. Paths compare in NFC, so a link typed on one
-- system finds a file named on another (macOS writes NFD).
CREATE VIEW resolved_links AS
SELECT l.doc_id, l.target, l.kind, l.text,
	CASE l.kind
		WHEN 'wikilink' THEN (SELECT d.path FROM documents d WHERE d.name = l.candidate
			ORDER BY length(d.path), d.path LIMIT 1)
		ELSE (SELECT d.path FROM documents d WHERE d.key = l.candidate ORDER BY d.path LIMIT 1)
	END AS target_path
FROM links l;
`

// triggers keep the full-text tables in step with documents. A full build creates them after filling the
// tables and running one rebuild per full-text table, which is much faster than indexing row by row.
const triggers = `
CREATE TRIGGER documents_insert AFTER INSERT ON documents BEGIN
	INSERT INTO fts (rowid, title, aliases, tags, description, headings, body)
		VALUES (new.id, new.title, new.aliases, new.tags, new.description, new.headings, new.body);
	INSERT INTO names (rowid, title, aliases, name)
		VALUES (new.id, new.title, new.aliases, new.name);
	INSERT INTO words (rowid, title, aliases, tags, description, headings, body)
		VALUES (new.id, new.title, new.aliases, new.tags, new.description, new.headings, new.body);
END;

CREATE TRIGGER documents_delete AFTER DELETE ON documents BEGIN
	INSERT INTO fts (fts, rowid, title, aliases, tags, description, headings, body)
		VALUES ('delete', old.id, old.title, old.aliases, old.tags, old.description, old.headings, old.body);
	INSERT INTO names (names, rowid, title, aliases, name)
		VALUES ('delete', old.id, old.title, old.aliases, old.name);
	INSERT INTO words (words, rowid, title, aliases, tags, description, headings, body)
		VALUES ('delete', old.id, old.title, old.aliases, old.tags, old.description, old.headings, old.body);
END;
`

// rebuildFullText fills the full-text tables from documents in one pass.
const rebuildFullText = `
INSERT INTO fts (fts) VALUES ('rebuild');
INSERT INTO names (names) VALUES ('rebuild');
INSERT INTO words (words) VALUES ('rebuild');
INSERT INTO fts (fts) VALUES ('optimize');
`
