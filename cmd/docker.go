package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"
)

// hakobuDockerSocket is the socket of the rootless Docker install.sh sets
// up for hakobu: a plain `docker` on the server talks to the system's
// daemon instead, which has none of hakobu's containers.
const hakobuDockerSocket = "/run/hakobu-docker/docker.sock"

var dockerCmd = &cobra.Command{
	Use:   "docker [docker arguments]",
	Short: "Run docker against hakobu's Docker, where its containers are",
	Long: `Run docker against hakobu's Docker, where its containers, volumes and
databases are. hakobu runs its own rootless Docker, which a plain docker
command on the server doesn't see. As root:

  /opt/hakobu/hakobu docker ps
  /opt/hakobu/hakobu docker exec -it hakobu-postgres psql -U postgres`,
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		bin, err := exec.LookPath("docker")
		if err != nil {
			return fmt.Errorf("no docker command on this server: %w", err)
		}
		host := os.Getenv("DOCKER_HOST")
		if host == "" {
			if _, err := os.Stat(hakobuDockerSocket); err == nil {
				host = "unix://" + hakobuDockerSocket
			}
		}
		env := os.Environ()
		if host != "" {
			env = append(env, "DOCKER_HOST="+host)
		}
		return syscall.Exec(bin, append([]string{"docker"}, args...), env)
	},
}

func init() {
	rootCmd.AddCommand(dockerCmd)
}
