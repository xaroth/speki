package index

/*
// The prototypes speki needs from the SQLite that go-sqlite3 compiles into the binary.
typedef struct sqlite3 sqlite3;
typedef struct sqlite3_api_routines sqlite3_api_routines;
int sqlite3_auto_extension(void (*entry)(void));
int sqlite3_db_config(sqlite3 *db, int op, ...);
const char *sqlite3_db_filename(sqlite3 *db, const char *name);
long long sqlite3_soft_heap_limit64(long long n);

#define SPEKI_DBCONFIG_DEFENSIVE 1010
#define SPEKI_DBCONFIG_TRUSTED_SCHEMA 1017

// spekiHarden runs for every new connection. Only in-memory connections (the query sandbox) are
// hardened: DEFENSIVE would stop index builds from turning the journal off.
static int spekiHarden(sqlite3 *db, char **err, const sqlite3_api_routines *api) {
	const char *name = sqlite3_db_filename(db, "main");
	if (name == 0 || name[0] == 0) {
		sqlite3_db_config(db, SPEKI_DBCONFIG_DEFENSIVE, 1, (int *)0);
		sqlite3_db_config(db, SPEKI_DBCONFIG_TRUSTED_SCHEMA, 0, (int *)0);
	}
	return 0;
}

static int spekiRegisterHarden(void) {
	return sqlite3_auto_extension((void (*)(void))spekiHarden);
}
*/
import "C"

import (
	"fmt"
	"sync"
)

// softHeapLimit is how much memory SQLite aims to stay under, in bytes, before it frees cache.
const softHeapLimit = 512 << 20

var harden = sync.OnceValue(func() error {
	rc := C.spekiRegisterHarden()
	if rc != 0 {
		return fmt.Errorf("register the sandbox hardening hook: sqlite error %d", rc)
	}
	C.sqlite3_soft_heap_limit64(softHeapLimit)
	return nil
})
