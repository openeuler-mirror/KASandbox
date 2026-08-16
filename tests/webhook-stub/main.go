// Command webhook-stub is an end-to-end stub test for the 9 internal E2B API
// endpoints consumed by the in-cluster webhook service. It simulates the
// webhook's call sequence: node-side actions (cri-multiplex admin.sock, Pod
// deletion) are only logged as [stub] lines, while all API calls are real HTTP
// requests against a locally started API instance.
package main

import (
	"flag"
	"fmt"
	"os"
)

type scenario struct {
	name string
	fn   func() error
}

func main() {
	apiURL := flag.String("api", "http://127.0.0.1:13200", "base URL of the API under test")
	token := flag.String("token", "test-admin-token", "admin token (X-Admin-Token header)")
	teamID := flag.String("team", "11111111-1111-1111-1111-111111111111", "seeded team UUID")
	baseEnv := flag.String("base-env", "stub-base-env-01", "seeded base template (envs.id)")
	flag.Parse()

	s := &state{
		client:  NewClient(*apiURL, *token),
		teamID:  *teamID,
		baseEnv: *baseEnv,
	}

	scenarios := []scenario{
		{"鉴权 (auth)", s.scenarioAuth},
		{"参数校验 (validation)", s.scenarioValidation},
		{"Pause 全流程", s.scenarioPauseFlow},
		{"Pause 复活语义", s.scenarioPauseRevive},
		{"build 状态幂等/冲突", s.scenarioBuildStatus},
		{"Checkpoint 全流程", s.scenarioCheckpointFlow},
		{"删除 checkpoint 模板", s.scenarioDeleteCheckpoint},
		{"删除 paused 沙箱", s.scenarioDeletePaused},
		{"错误路径", s.scenarioErrorPaths},
	}

	fmt.Printf("webhook-stub: api=%s team=%s base-env=%s\n", *apiURL, *teamID, *baseEnv)

	passed := 0
	for i, sc := range scenarios {
		fmt.Printf("==> [%d/%d] %s\n", i+1, len(scenarios), sc.name)
		if err := sc.fn(); err != nil {
			fmt.Printf("FAIL [%d/%d] %s: %v\n", i+1, len(scenarios), sc.name, err)
			fmt.Printf("\nSUMMARY: %d/%d scenarios passed, 1 failed, %d not run\n", passed, len(scenarios), len(scenarios)-passed-1)
			os.Exit(1)
		}
		passed++
	}

	fmt.Printf("\nSUMMARY: all %d/%d scenarios passed\n", passed, len(scenarios))
}
