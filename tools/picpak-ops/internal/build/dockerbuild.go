package build

import "strings"

// dockerArgv assembles the containerized build invocation from config (Policy=Data —
// no docker flag, image, target, or mount is a code literal). The shape is:
//
//	{docker_cmd} {docker_run_args…} {docker_image} {build_cmd…}
//
// with {repo}→the absolute repo root (an absolute bind-mount source), {name}→the
// expanded container name (so the docker-kill cancel fallback can reap it), and
// {target}→the idf target inside build_cmd. It returns the argv and the resolved
// container name.
func dockerArgv(cfg Config, runid string) (argv []string, containerName string) {
	containerName = expandTmpl(cfg.ContainerNameTmpl, map[string]string{"runid": runid})

	repl := map[string]string{
		"repo":   cfg.RepoAbs,
		"name":   containerName,
		"image":  cfg.DockerImage,
		"target": cfg.Target,
	}

	argv = append(argv, cfg.DockerCmd)
	for _, a := range cfg.DockerRunArgs {
		argv = append(argv, expandTmpl(a, repl))
	}
	argv = append(argv, cfg.DockerImage)
	for _, a := range cfg.BuildCmd {
		argv = append(argv, expandTmpl(a, repl))
	}
	return argv, containerName
}

// dockerKillArgv is the cancel fallback: kill the named container directly (the
// run --rm cleans it up on SIGKILL of the client, but over ssh a named kill within
// cancel_kill_grace_ms reaps an orphaned multi-minute build host-side). Covers ONLY
// the docker stage.
func dockerKillArgv(cfg Config, containerName string) []string {
	return []string{cfg.DockerCmd, "kill", containerName}
}

// expandTmpl replaces each {key} token in s with repl[key]. An unknown token is left
// verbatim (so a stray brace never silently vanishes).
func expandTmpl(s string, repl map[string]string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	for k, v := range repl {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}
