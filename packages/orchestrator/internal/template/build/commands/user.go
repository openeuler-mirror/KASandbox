package commands

import (
	"context"
	"fmt"

	"go.uber.org/zap/zapcore"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/proxy"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/sandboxtools"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

type User struct{}

var _ Command = (*User)(nil)

func (u *User) Execute(
	ctx context.Context,
	logger logger.Logger,
	lvl zapcore.Level,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	prefix string,
	step *templatemanager.TemplateStep,
	cmdMetadata metadata.Context,
) (metadata.Context, error) {
	args := step.GetArgs()
	// args: [username, optional_add_to_sudo]
	if len(args) < 1 {
		return metadata.Context{}, fmt.Errorf("USER requires a username argument")
	}

	userArg := args[0]

	// Check if user already exists
	err := sandboxtools.RunCommand(
		ctx,
		proxy,
		sandboxID,
		fmt.Sprintf("id -u %s", userArg),
		metadata.Context{
			User:    "root",
			EnvVars: cmdMetadata.EnvVars,
		},
	)
	userExists := err == nil

	// Only create user if it doesn't exist
	if !userExists {
		// 检测系统类型：Debian/Ubuntu 使用 adduser，RHEL/CentOS/openEuler 使用 useradd
		var createUserCmd string

		// 通过检查 /etc/debian_version 文件判断是否为 Debian 系
		checkDebian := sandboxtools.RunCommand(
			ctx,
			proxy,
			sandboxID,
			"test -f /etc/debian_version",
			metadata.Context{
				User:    "root",
				EnvVars: cmdMetadata.EnvVars,
			},
		)

		if checkDebian == nil {
			// Debian/Ubuntu: adduser 是 Perl 脚本，支持 --disabled-password 和 --gecos
			createUserCmd = fmt.Sprintf("adduser --disabled-password --gecos \"\" %s", userArg)
		} else {
			// openEuler/RHEL/CentOS: 使用 useradd，-m 创建家目录，-s 指定 shell，-c 对应 gecos
			createUserCmd = fmt.Sprintf("useradd -m -s /bin/bash -c \"\" %s", userArg)
		}

		err = sandboxtools.RunCommandWithLogger(
			ctx,
			proxy,
			logger,
			lvl,
			prefix,
			sandboxID,
			createUserCmd,
			metadata.Context{
				User:    "root",
				EnvVars: cmdMetadata.EnvVars,
			},
		)
		if err != nil {
			return metadata.Context{}, fmt.Errorf("failed to create user: %w", err)
		}
	}

	if len(args) > 1 && args[1] == "true" {
		cmdMetadata, err = addToSudoers(ctx, logger, proxy, sandboxID, prefix, zapcore.DebugLevel, cmdMetadata, userArg)
		if err != nil {
			return metadata.Context{}, err
		}
	}

	return saveUserMeta(ctx, proxy, sandboxID, cmdMetadata, userArg)
}

func addToSudoers(
	ctx context.Context,
	logger logger.Logger,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	prefix string,
	lvl zapcore.Level,
	cmdMetadata metadata.Context,
	userArg string,
) (metadata.Context, error) {
	// 检测系统类型并选择对应的 sudo 组
	var sudoGroup string

	// 检查 wheel 组是否存在（openEuler/CentOS/RHEL）
	checkErr := sandboxtools.RunCommand(
		ctx,
		proxy,
		sandboxID,
		"getent group wheel",
		metadata.Context{
			User:    "root",
			EnvVars: cmdMetadata.EnvVars,
		},
	)

	if checkErr == nil {
		sudoGroup = "wheel"
	} else {
		// 默认使用 sudo 组（Ubuntu/Debian）
		sudoGroup = "sudo"
	}

	// 添加用户到对应的 sudo 组
	err := sandboxtools.RunCommandWithLogger(
		ctx,
		proxy,
		logger,
		lvl,
		prefix,
		sandboxID,
		fmt.Sprintf("usermod -aG %s %s", sudoGroup, userArg),
		metadata.Context{
			User:    "root",
			EnvVars: cmdMetadata.EnvVars,
		},
	)
	if err != nil {
		return metadata.Context{}, fmt.Errorf("failed to add user to %s group: %w", sudoGroup, err)
	}

	// Remove password
	err = sandboxtools.RunCommandWithLogger(
		ctx,
		proxy,
		logger,
		lvl,
		prefix,
		sandboxID,
		fmt.Sprintf("passwd -d %s", userArg),
		metadata.Context{
			User:    "root",
			EnvVars: cmdMetadata.EnvVars,
		},
	)
	if err != nil {
		return metadata.Context{}, fmt.Errorf("failed to remove user password: %w", err)
	}

	// Add to sudoers if not already present
	err = sandboxtools.RunCommandWithLogger(
		ctx,
		proxy,
		logger,
		lvl,
		prefix,
		sandboxID,
		fmt.Sprintf("grep -q '^%s ALL=(ALL:ALL) NOPASSWD: ALL' /etc/sudoers || echo '%s ALL=(ALL:ALL) NOPASSWD: ALL' >>/etc/sudoers", userArg, userArg),
		metadata.Context{
			User:    "root",
			EnvVars: cmdMetadata.EnvVars,
		},
	)
	if err != nil {
		return metadata.Context{}, fmt.Errorf("failed to configure sudoers: %w", err)
	}

	return cmdMetadata, nil
}

func saveUserMeta(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	cmdMetadata metadata.Context,
	user string,
) (metadata.Context, error) {
	err := sandboxtools.RunCommandWithOutput(
		ctx,
		proxy,
		sandboxID,
		fmt.Sprintf(`printf "%s"`, user),
		metadata.Context{
			User: "root",
		},
		func(stdout, _ string) {
			user = stdout
		},
	)

	cmdMetadata.User = user

	return cmdMetadata, err
}
