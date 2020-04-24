package pgstore

import (
	"database/sql"
	"encoding/base32"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"

	_ "github.com/jackc/pgx/stdlib"
	"github.com/jmoiron/sqlx"
)

type logger interface {
	Infof(string, ...interface{})
	Warningf(string, ...interface{})
	Errorf(string, ...interface{})
}

type defaultLogger struct{}

func (l defaultLogger) Infof(format string, v ...interface{}) {
	log.Printf(format, v...)
}

func (l defaultLogger) Warningf(format string, v ...interface{}) {
	log.Printf(format, v...)
}

func (l defaultLogger) Errorf(format string, v ...interface{}) {
	log.Printf(format, v...)
}

// PGStore represents the currently configured session store.
type PGStore struct {
	Codecs  []securecookie.Codec
	Options *sessions.Options
	Path    string
	db      *sqlx.DB
	logger  logger
}

// PGSession type
type PGSession struct {
	ID         int64     `db:"id"`
	Key        string    `db:"key"`
	Data       string    `db:"data"`
	CreatedOn  time.Time `db:"created_on"`
	ModifiedOn time.Time `db:"modified_on"`
	ExpiresOn  time.Time `db:"expires_on"`
}

// NewPGStore creates a new PGStore instance and a new database/sql pool.
// This will also create in the database the schema needed by pgstore.
func NewPGStore(dbURL string, opts *sessions.Options, logger logger, keyPairs ...[]byte) (*PGStore, error) {
	db, err := sqlx.Open("pgx", dbURL)
	if err != nil {
		return nil, err
	}
	return NewPGStoreFromPool(db, opts, logger, keyPairs...)
}

// NewPGStoreFromPool creates a new PGStore instance from an existing
// database/sql pool.
// This will also create the database schema needed by pgstore.
func NewPGStoreFromPool(db *sqlx.DB, opts *sessions.Options, logger logger, keyPairs ...[]byte) (*PGStore, error) {
	if logger == nil {
		logger = defaultLogger{}
	}
	p := &PGStore{
		Codecs:  securecookie.CodecsFromPairs(keyPairs...),
		Options: opts,
		db:      db,
	}

	// Create table if it doesn't exist
	err := p.createSessionsTable()
	if err != nil {
		return nil, err
	}

	return p, nil
}

// Close closes the database connection.
func (p *PGStore) Close() {
	p.db.Close()
}

// Get Fetches a session for a given name after it has been added to the
// registry.
func (p *PGStore) Get(r *http.Request, name string) (*sessions.Session, error) {
	return sessions.GetRegistry(r).Get(p, name)
}

// New returns a new session for the given name without adding it to the registry.
func (p *PGStore) New(r *http.Request, name string) (*sessions.Session, error) {
	session := sessions.NewSession(p, name)
	if session == nil {
		return nil, nil
	}

	opts := *p.Options
	session.Options = &(opts)
	session.IsNew = true

	var err error
	if c, errCookie := r.Cookie(name); errCookie == nil {
		err = securecookie.DecodeMulti(name, c.Value, &session.ID, p.Codecs...)
		if err == nil {
			err = p.load(session)
			if err == nil {
				session.IsNew = false
			} else if errors.Cause(err) == sql.ErrNoRows {
				err = nil
			}
		}
	}

	p.MaxAge(p.Options.MaxAge)

	return session, err
}

// Save saves the given session into the database and deletes cookies if needed
func (p *PGStore) Save(r *http.Request, w http.ResponseWriter, session *sessions.Session) error {
	// Set delete if max-age is < 0
	if session.Options.MaxAge < 0 {
		if err := p.destroy(session); err != nil {
			return err
		}
		http.SetCookie(w, sessions.NewCookie(session.Name(), "", session.Options))
		return nil
	}

	if session.ID == "" {
		// Generate a random session ID key suitable for storage in the DB
		session.ID = strings.TrimRight(
			base32.StdEncoding.EncodeToString(
				securecookie.GenerateRandomKey(32),
			), "=")
	}

	if err := p.save(session); err != nil {
		return err
	}

	// Keep the session ID key in a cookie so it can be looked up in DB later.
	encoded, err := securecookie.EncodeMulti(session.Name(), session.ID, p.Codecs...)
	if err != nil {
		return err
	}

	http.SetCookie(w, sessions.NewCookie(session.Name(), encoded, session.Options))
	return nil
}

// MaxLength restricts the maximum length of new sessions to l.
// If l is 0 there is no limit to the size of a session, use with caution.
// The default for a new PGStore is 4096. PostgreSQL allows for max
// value sizes of up to 1GB (http://www.postgresql.org/docs/current/interactive/datatype-character.html)
func (p *PGStore) MaxLength(l int) {
	for _, c := range p.Codecs {
		if codec, ok := c.(*securecookie.SecureCookie); ok {
			codec.MaxLength(l)
		}
	}
}

// MaxAge sets the maximum age for the store and the underlying cookie
// implementation. Individual sessions can be deleted by setting Options.MaxAge
// = -1 for that session.
func (p *PGStore) MaxAge(age int) {
	p.Options.MaxAge = age

	// Set the maxAge for each securecookie instance.
	for _, codec := range p.Codecs {
		if sc, ok := codec.(*securecookie.SecureCookie); ok {
			sc.MaxAge(age)
		}
	}
}

// load fetches a session by ID from the database and decodes its content
// into session.Values.
func (p *PGStore) load(session *sessions.Session) error {
	s := PGSession{}

	err := p.db.Get(&s, "SELECT id, key, data, created_on, modified_on, expires_on FROM http_sessions WHERE key = $1", session.ID)
	if err != nil {
		return errors.Wrapf(err, "Unable to find session in the database")
	}

	return securecookie.DecodeMulti(session.Name(), string(s.Data), &session.Values, p.Codecs...)
}

// save writes encoded session.Values to a database record.
// writes to http_sessions table by default.
func (p *PGStore) save(session *sessions.Session) error {
	encoded, err := securecookie.EncodeMulti(session.Name(), session.Values, p.Codecs...)
	if err != nil {
		return err
	}

	crOn := session.Values["created_on"]
	exOn := session.Values["expires_on"]

	var expiresOn time.Time

	createdOn, ok := crOn.(time.Time)
	if !ok {
		createdOn = time.Now()
	}

	if exOn == nil {
		expiresOn = time.Now().Add(time.Second * time.Duration(session.Options.MaxAge))
	} else {
		expiresOn = exOn.(time.Time)
		if expiresOn.Sub(time.Now().Add(time.Second*time.Duration(session.Options.MaxAge))) < 0 {
			expiresOn = time.Now().Add(time.Second * time.Duration(session.Options.MaxAge))
		}
	}

	s := PGSession{
		Key:        session.ID,
		Data:       encoded,
		CreatedOn:  createdOn,
		ExpiresOn:  expiresOn,
		ModifiedOn: time.Now(),
	}

	if session.IsNew {
		return p.insert(&s)
	}

	return p.update(&s)
}

// Delete session
func (p *PGStore) destroy(session *sessions.Session) error {
	_, err := p.db.Exec("DELETE FROM http_sessions WHERE key = $1", session.ID)
	return err
}

func (p *PGStore) createSessionsTable() error {
	stmt := `
	DO $$
		BEGIN
			CREATE TABLE IF NOT EXISTS http_sessions (
				id BIGSERIAL PRIMARY KEY,
				key BYTEA,
				data BYTEA,
				created_on TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
				modified_on TIMESTAMPTZ,
				expires_on TIMESTAMPTZ
			);
			CREATE INDEX IF NOT EXISTS http_sessions_expiry_idx ON http_sessions (expires_on);
			CREATE INDEX IF NOT EXISTS http_sessions_key_idx ON http_sessions (key);		
			EXCEPTION WHEN insufficient_privilege THEN
				IF NOT EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = current_schema() AND tablename = 'http_sessions') THEN
					RAISE;
				END IF;
			WHEN others THEN RAISE;
		END;
	$$;
	`
	_, err := p.db.Exec(stmt)
	if err != nil {
		return errors.Wrapf(err, "Unable to create http_sessions table in the database")
	}

	return nil
}

func (p *PGStore) insert(s *PGSession) error {
	stmt := `INSERT INTO http_sessions (key, data, created_on, modified_on, expires_on)
           VALUES ($1, $2, $3, $4, $5)`
	_, err := p.db.Exec(stmt, s.Key, s.Data, s.CreatedOn, s.ModifiedOn, s.ExpiresOn)

	return err
}

func (p *PGStore) update(s *PGSession) error {
	stmt := `UPDATE http_sessions SET data=$1, modified_on=$2, expires_on=$3 WHERE key=$4`
	_, err := p.db.Exec(stmt, s.Data, s.ModifiedOn, s.ExpiresOn, s.Key)
	return err
}
