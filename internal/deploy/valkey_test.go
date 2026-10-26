package deploy

import "testing"

func TestValkeyMemory(t *testing.T) {
	info := "# Memory\r\nused_memory:1048576\r\nused_memory_human:1.00M\r\nmaxmemory:67108864\r\nmaxmemory_human:64.00M\r\n"
	used, limit, err := valkeyMemory(info)
	if err != nil || used != 1<<20 || limit != 64<<20 {
		t.Errorf("%d, %d, %v", used, limit, err)
	}
	if _, _, err := valkeyMemory("NOAUTH"); err == nil {
		t.Error("no error without used_memory")
	}
}

func TestMountOf(t *testing.T) {
	info := `22 1 8:1 / / rw - ext4 /dev/sda1 rw
30 22 8:2 / /home rw - ext4 /dev/sda2 rw
31 22 8:3 / /homework rw - ext4 /dev/sda3 rw`
	for path, want := range map[string]string{
		"/home/hakobu-docker/.local/share/docker": "/home",
		"/var/lib/docker":                         "/",
		"/homeworkx/a":                            "/",
		"/home":                                   "/home",
	} {
		if got := mountOf(path, info); got != want {
			t.Errorf("mountOf(%s) = %s, want %s", path, got, want)
		}
	}
}
