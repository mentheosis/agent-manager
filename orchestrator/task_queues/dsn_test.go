package taskqueues

import (
	"strings"
	"testing"
)

func TestDatabaseDSNFormats(t *testing.T) {
	for _, raw := range []string{"user:secret@tcp(staked-db:3306)/coinstakes", "mysql://user:secret@staked-db:3306/coinstakes", " mysql://user:secret@staked-db/coinstakes "} {
		c, err := parseDatabaseDSN(raw)
		if err != nil {
			t.Fatal(err)
		}
		if c.User != "user" || c.Passwd != "secret" || c.Addr != "staked-db:3306" || c.DBName != "coinstakes" {
			t.Fatal("connection fields differ")
		}
	}
	c, err := parseDatabaseDSN("mysql://user:p%40ss%3A%2F%2Fword@[::1]:3307/audit?parseTime=true&timeout=5s")
	if err != nil {
		t.Fatal(err)
	}
	if c.Passwd != "p@ss://word" || c.Addr != "[::1]:3307" || !c.ParseTime || c.Timeout.Seconds() != 5 {
		t.Fatal("URL decoding/options failed")
	}
	c, err = parseDatabaseDSN("user:secret://value@tcp(localhost:3306)/audit")
	if err != nil || c.Passwd != "secret://value" {
		t.Fatal("native password altered")
	}
}

func TestDatabaseDSNRejectsInvalidWithoutSecrets(t *testing.T) {
	for _, raw := range []string{"postgres://user:secret@host/db", "mysql://user:secret@/db", "mysql://user:secret@host", "mysql://user:secret@host/db?timeout=invalid", "mysql://user:secret%ZZ@host/db"} {
		_, err := parseDatabaseDSN(raw)
		if err == nil {
			t.Fatal("expected rejection")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("error exposed credentials")
		}
	}
}
