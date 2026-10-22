package index

/*
// The prototypes speki needs from the SQLite that go-sqlite3 compiles into the binary.
typedef struct sqlite3 sqlite3;
typedef struct sqlite3_api_routines sqlite3_api_routines;
int sqlite3_auto_extension(void (*entry)(void));
int sqlite3_db_config(sqlite3 *db, int op, ...);
const char *sqlite3_db_filename(sqlite3 *db, const char *name);
long long sqlite3_soft_heap_limit64(long long n);
long long sqlite3_hard_heap_limit64(long long n);
long long sqlite3_memory_used(void);
void sqlite3_progress_handler(sqlite3 *db, int n, int (*handler)(void *), void *arg);

#define SPEKI_DBCONFIG_DEFENSIVE 1010
#define SPEKI_DBCONFIG_TRUSTED_SCHEMA 1017

static volatile long long spekiQueryBudget;

// spekiOverBudget interrupts a sandbox query once SQLite's memory, for the whole process, passes the
// query budget: below the hard limit, so searches and index builds still get memory.
static int spekiOverBudget(void *arg) {
	return sqlite3_memory_used() > spekiQueryBudget;
}

static void spekiSetQueryBudget(long long n) { spekiQueryBudget = n; }
static int spekiOverQueryBudget(void) { return spekiOverBudget(0); }

// spekiHarden runs for every new connection. Only in-memory connections (the query sandbox) are
// hardened: DEFENSIVE would stop index builds from turning the journal off.
static int spekiHarden(sqlite3 *db, char **err, const sqlite3_api_routines *api) {
	const char *name = sqlite3_db_filename(db, "main");
	if (name == 0 || name[0] == 0) {
		sqlite3_db_config(db, SPEKI_DBCONFIG_DEFENSIVE, 1, (int *)0);
		sqlite3_db_config(db, SPEKI_DBCONFIG_TRUSTED_SCHEMA, 0, (int *)0);
		sqlite3_progress_handler(db, 1000, spekiOverBudget, 0);
	}
	return 0;
}

static int spekiRegisterHarden(void) {
	return sqlite3_auto_extension((void (*)(void))spekiHarden);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"

	"github.com/mattn/go-sqlite3"
)

// The default memory limits, in MiB. See SetMemoryLimits.
const (
	DefaultMemoryLimit = 1024
	DefaultQueryMemory = 384
)

func init() { SetMemoryLimits(DefaultMemoryLimit, DefaultQueryMemory) }

// SetMemoryLimits sets SQLite's memory limits for the whole process, in MiB. At limit an allocation fails
// with SQLITE_NOMEM: that's what stops one query from taking the process down, since in-memory sorts and
// recursive queries can't spill to disk. Above half of it, SQLite frees cache. Sandbox queries are
// interrupted past query, which leaves the rest for search and indexing. Set them before using indexes.
func SetMemoryLimits(limit, query int64) {
	setHardHeapLimit(limit << 20) // first: SQLite caps the soft limit at the hard limit in force
	C.sqlite3_soft_heap_limit64(C.longlong(limit << 19))
	setQueryMemoryBudget(query << 20)
}

var harden = sync.OnceValue(func() error {
	rc := C.spekiRegisterHarden()
	if rc != 0 {
		return fmt.Errorf("register the sandbox hardening hook: sqlite error %d", rc)
	}
	return nil
})

// setHardHeapLimit changes the hard limit. Tests lower it to try a query that runs into it.
func setHardHeapLimit(n int64) { C.sqlite3_hard_heap_limit64(C.longlong(n)) }

// setQueryMemoryBudget changes the sandbox budget. Tests lower it.
func setQueryMemoryBudget(n int64) { C.spekiSetQueryBudget(C.longlong(n)) }

// overQueryBudget reports whether SQLite's memory is past the sandbox budget.
func overQueryBudget() bool { return C.spekiOverQueryBudget() != 0 }

// OutOfMemory reports whether err is SQLite running into the hard heap limit. Other work, such as a
// heavy query, can cause it, so it passes on its own.
func OutOfMemory(err error) bool {
	serr, ok := errors.AsType[sqlite3.Error](err)
	return ok && serr.Code == sqlite3.ErrNomem
}
