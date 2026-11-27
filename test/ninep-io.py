"""Regression checks for a mounted volume; only a new temporary directory is used."""

import mmap
import multiprocessing
import os
import sqlite3
import sys
import tempfile
import time
import unittest


def write_transactions(path, worker):
    con = sqlite3.connect(path, timeout=1)
    try:
        con.execute("PRAGMA synchronous=FULL")
        deadline = time.monotonic() + 90
        for seq in range(10):
            while True:
                try:
                    with con:
                        con.execute(
                            "INSERT INTO t VALUES (?, ?, ?)",
                            (worker, seq, bytes([worker, seq]) * 512),
                        )
                    break
                except sqlite3.OperationalError as err:
                    # Writer contention is expected; never retry I/O errors.
                    if (err.sqlite_errorcode & 255) not in (sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED):
                        raise
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(0.01 * (worker + 1))
    finally:
        con.close()


class VolumeIOTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="zerofs-io-", dir=ROOT)
        self.addCleanup(self.directory.cleanup)

    def path(self, name):
        return os.path.join(self.directory.name, name)

    def test_write_fsync_rename_and_truncate(self):
        path = self.path("file")
        payload = bytes(range(256)) * 1024
        with open(path, "wb") as f:
            f.write(payload)
            f.flush()
            os.fsync(f.fileno())
        os.rename(path, path + "-renamed")
        with open(path + "-renamed", "r+b") as f:
            self.assertEqual(f.read(), payload)
            f.truncate(8192)
            os.fsync(f.fileno())
        self.assertEqual(os.stat(path + "-renamed").st_size, 8192)

    def test_shared_writable_mmap(self):
        path = self.path("shm")
        with open(path, "w+b") as f:
            f.truncate(32768)
            with mmap.mmap(f.fileno(), 32768, access=mmap.ACCESS_WRITE) as mapping:
                mapping[:4] = b"test"
                mapping.flush()
            os.fsync(f.fileno())
        with open(path, "rb") as f:
            self.assertEqual(f.read(4), b"test")

    def check_sqlite(self, mode):
        path = self.path(mode + ".db")
        con = sqlite3.connect(path)
        try:
            self.assertEqual(con.execute("PRAGMA journal_mode=" + mode).fetchone()[0], mode)
            con.execute("PRAGMA synchronous=FULL")
            con.execute("CREATE TABLE t(worker INT, seq INT, payload BLOB, PRIMARY KEY(worker, seq))")
        finally:
            con.close()

        # fork works when the script is streamed through kubectl exec's stdin.
        ctx = multiprocessing.get_context("fork")
        workers = [ctx.Process(target=write_transactions, args=(path, i)) for i in range(4)]
        try:
            for worker in workers:
                worker.start()
            for worker in workers:
                worker.join(timeout=100)
                self.assertEqual(worker.exitcode, 0, "SQLite worker failed or timed out")
        finally:
            for worker in workers:
                if worker.is_alive():
                    worker.terminate()
                    worker.join(timeout=5)

        # Reopen after every writer exits; verify contents as well as structure.
        con = sqlite3.connect(path)
        try:
            self.assertEqual(con.execute("PRAGMA integrity_check").fetchall(), [("ok",)])
            rows = con.execute("SELECT worker, seq, payload FROM t").fetchall()
            self.assertEqual(len(rows), 40)
            for worker, seq, payload in rows:
                self.assertEqual(payload, bytes([worker, seq]) * 512)
        finally:
            con.close()

    def test_sqlite_wal(self):
        self.check_sqlite("wal")

    def test_sqlite_rollback_journal(self):
        self.check_sqlite("delete")


if __name__ == "__main__":
    ROOT = sys.argv[1]
    print("Volume I/O checks on", ROOT, "with SQLite", sqlite3.sqlite_version, flush=True)
    unittest.main(argv=[sys.argv[0]], verbosity=2)
