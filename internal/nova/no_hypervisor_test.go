package nova

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// SPEC-031 FR-019: real mode never reports a server it did not create. Without a libvirt
// connection a create used to take the stub branch and turn ACTIVE with no guest behind it.

const notReadyFault = "the hypervisor is not ready to start guests"

// newNoHypervisorService builds a real-mode compute service whose hypervisor is mgr (nil: none
// wired in), with a router like newBootService's.
func newNoHypervisorService(t *testing.T, mgr *hypervisor.VMManager) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := NewService(testPool, "", "real", nil)
	if mgr != nil {
		svc.vmManager = mgr
	}
	t.Cleanup(svc.Shutdown)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set("user_id", u)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	svc.RegisterRoutes(&r.RouterGroup)
	return r
}

// unconnectedManager is the hypervisor of a real-mode server whose libvirt socket does not
// exist. The socket is explicit: this package reaches neither the fake libvirtd nor the
// automatic choice's run directory.
func unconnectedManager(t *testing.T) *hypervisor.VMManager {
	t.Helper()
	dir := t.TempDir()
	m, err := hypervisor.StartVMManager(filepath.Join(dir, "missing-sock"), "real", filepath.Join(dir, "console-sockets"))
	if err != nil {
		t.Fatalf("StartVMManager: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// useTempStatePath keeps per-instance directories out of /var/lib/lightstack for the test.
func useTempStatePath(t *testing.T) {
	t.Helper()
	prev := paths.Base()
	paths.SetBase(t.TempDir())
	t.Cleanup(func() { paths.SetBase(prev) })
}

// seedBootableVolume inserts an available, bootable, unattached volume in the seeded project.
// Booting from it reaches the hypervisor without staging an image disk with qemu-img first.
func seedBootableVolume(t *testing.T, name string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO volumes (id, name, project_id, size_gb, status, bootable)
		 VALUES ($1, $2, $3, 1, 'available', true)`, id, name, seededProjectID); err != nil {
		t.Fatalf("seeding volume: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volume_attachments WHERE volume_id = $1`, id)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, id)
	})
	return id
}

// createFromVolume posts a server booting from volumeID and returns the new server's id.
func createFromVolume(t *testing.T, r *gin.Engine, name, volumeID string) string {
	t.Helper()
	cleanupServerNamed(t, name)
	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"00000000-0000-0000-0000-000000000011",`+
		`"block_device_mapping_v2":[{"uuid":%q,"boot_index":0,"source_type":"volume","destination_type":"volume"}]}}`,
		name, volumeID)
	code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusAccepted {
		t.Fatalf("create: %d %s; want 202", code, resp)
	}
	return createdServerID(t, resp)
}

// serverFault reads the fault a server reports.
func serverFault(t *testing.T, r *gin.Engine, id string) (code int, message string) {
	t.Helper()
	status, resp := doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID))
	if status != http.StatusOK {
		t.Fatalf("show %s: %d %s", id, status, resp)
	}
	var body struct {
		Server struct {
			Fault *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"fault"`
		} `json:"server"`
	}
	if err := json.Unmarshal([]byte(resp), &body); err != nil {
		t.Fatalf("decoding show: %v", err)
	}
	if body.Server.Fault == nil {
		t.Fatalf("server %s reports no fault; body %s", id, resp)
	}
	return body.Server.Fault.Code, body.Server.Fault.Message
}

func TestNoHypervisorCreateEndsError(t *testing.T) {
	requireDB(t)
	useTempStatePath(t)

	t.Run("unconnected manager", func(t *testing.T) {
		r := newNoHypervisorService(t, unconnectedManager(t))
		const name = "spec031-unconnected"
		id := createFromVolume(t, r, name, seedBootableVolume(t, name))
		if got := awaitInstanceStatus(t, name, "ERROR"); got != "ERROR" {
			t.Fatalf("status %q; want ERROR, never ACTIVE", got)
		}
		if code, msg := serverFault(t, r, id); code != http.StatusInternalServerError || msg != notReadyFault {
			t.Errorf("fault %d %q; want 500 %q", code, msg, notReadyFault)
		}
	})

	t.Run("no manager wired in", func(t *testing.T) {
		r := newNoHypervisorService(t, nil)
		const name = "spec031-no-manager"
		id := createFromVolume(t, r, name, seedBootableVolume(t, name))
		if got := awaitInstanceStatus(t, name, "ERROR"); got != "ERROR" {
			t.Fatalf("status %q; want ERROR, never ACTIVE", got)
		}
		if code, msg := serverFault(t, r, id); code != http.StatusInternalServerError || msg != notReadyFault {
			t.Errorf("fault %d %q; want 500 %q", code, msg, notReadyFault)
		}
	})
}

// A server action without a libvirt connection answers as during a lost connection.
func TestNoHypervisorActionAnswersGeneric500(t *testing.T) {
	requireDB(t)
	r := newNoHypervisorService(t, unconnectedManager(t))

	var id string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state, libvirt_domain_id)
		 VALUES ('spec031-stop', $1, $2, (SELECT id FROM flavors WHERE name = 'm1.small'), 'ACTIVE', 1, $3)
		 RETURNING id::text`,
		seededProjectID, seededUserID, uuid.NewString(),
	).Scan(&id); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, id)
	})

	code, resp := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-stop":null}`,
		testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusInternalServerError || !strings.Contains(resp, "failed to perform") {
		t.Errorf("os-stop: %d %s; want 500 \"failed to perform ...\"", code, resp)
	}
}

// The guarantee the remaining nil-manager branches rely on: in real mode InitHypervisor always
// leaves a manager, connected or not; only a refused URI fails it.
func TestInitHypervisorRealModeAlwaysHasAManager(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(nil, filepath.Join(dir, "missing-sock"), "real", nil)
	if err := svc.InitHypervisor(filepath.Join(dir, "cs")); err != nil {
		t.Fatalf("InitHypervisor with no libvirt socket: %v; want an unconnected manager", err)
	}
	if svc.vmManager == nil {
		t.Fatal("real mode left without a hypervisor manager")
	}
	svc.vmManager.Close()

	refused := NewService(nil, "qemu+tcp://h/system", "real", nil)
	if err := refused.InitHypervisor(filepath.Join(dir, "cs")); err == nil {
		t.Error("InitHypervisor accepted qemu+tcp://h/system")
	}
}

// createFault's messages are curated: the hypervisor's own error stays in the log.
func TestCreateFault(t *testing.T) {
	if got := createFault(fmt.Errorf("start: %w", hypervisor.ErrConsoleSocketDirNotReady)); got != notReadyFault {
		t.Errorf("createFault(console dir not ready) = %q; want %q", got, notReadyFault)
	}
	if got, want := createFault(errors.New("libvirt: boom")), "the hypervisor could not create the guest"; got != want {
		t.Errorf("createFault(other) = %q; want %q", got, want)
	}
}
