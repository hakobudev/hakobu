package backups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/s3"
)

func TestMain(m *testing.M) {
	deploy.RunDialerIfChild() // rootless Docker: this binary is the dialer too
	os.Exit(m.Run())
}

// HAKOBU_DOCKER_TEST=1 go test ./internal/backups -run Docker -v
//
// A real database backed up to a real S3 server (RustFS), the table
// dropped, and the backup restored.
func TestDockerBackupRestore(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	b := make([]byte, 3)
	rand.Read(b)
	id := "zt" + hex.EncodeToString(b)
	ctx := context.Background()
	oldPG := node.PostgresContainer
	node.PostgresContainer = id + "-postgres"
	t.Cleanup(func() {
		_ = deploy.RemoveContainer(ctx, node.PostgresContainer)
		_ = deploy.RemoveVolume(ctx, node.PostgresContainer+"_data")
		_ = exec.Command("docker", "rm", "-f", id+"-s3").Run()
		node.PostgresContainer = oldPG
	})

	port := freePort(t)
	if out, err := exec.Command("docker", "run", "-d", "--name", id+"-s3", "-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"-e", "RUSTFS_ACCESS_KEY=hakobu", "-e", "RUSTFS_SECRET_KEY=hakobu-secret", "rustfs/rustfs:latest").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	store := s3.New(fmt.Sprintf("http://127.0.0.1:%d", port), "us-east-1", "backups", "hakobu", "hakobu-secret")
	var err error
	for range 30 {
		if err = store.CreateBucket(ctx); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal("the S3 server didn't come up:", err)
	}

	n := node.Local{}
	if err := n.EnsurePostgres(ctx); err != nil {
		t.Fatal(err)
	}
	db := node.DBSpec{Name: id + "_main", User: id + "_main", Password: "pw" + id}
	if err := n.EnsureDatabase(ctx, db); err != nil {
		t.Fatal(err)
	}
	sql := func(q string) string {
		rows, err := deploy.PostgresQueryIn(ctx, node.PostgresContainer, db.Name, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, strings.Join(r, ","))
		}
		return strings.Join(out, ";")
	}
	sql(fmt.Sprintf(`CREATE TABLE cars (plate text); INSERT INTO cars VALUES ('AA1234'), ('BB5678'); ALTER TABLE cars OWNER TO "%s"`, db.User))

	server, _ := age.GenerateX25519Identity()
	owner, _ := age.GenerateX25519Identity()
	v := &Vault{
		Source: n, Primary: Target{Name: "rustfs", Store: store, Prefix: "hakobu/test/"},
		Recipients: []string{server.Recipient().String(), owner.Recipient().String()},
		Identity:   server, PartSize: 4 << 10, TempDir: t.TempDir(),
	}
	m, err := v.BackupDatabase(ctx, "kasl", "main", db)
	if err != nil {
		t.Fatal(err)
	}
	if m.Checked != "1 tables" {
		t.Errorf("checked %q", m.Checked)
	}
	sql("DROP TABLE cars")

	list, err := v.List(ctx, "kasl")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v, %v", list, err)
	}
	v.Identity = owner // as on a new server, with the owner's key
	if err := v.RestoreDatabase(ctx, list[0], db); err != nil {
		t.Fatal(err)
	}
	if got := sql("SELECT plate FROM cars ORDER BY plate"); got != "AA1234;BB5678" {
		t.Errorf("restored %q", got)
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
