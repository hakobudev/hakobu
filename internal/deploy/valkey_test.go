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
