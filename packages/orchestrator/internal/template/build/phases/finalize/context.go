package finalize

import (
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
)

func normalizeTemplateContext(meta metadata.Template, osType vmm.OsType) metadata.Template {
	if osType != vmm.OsWindows && osType != vmm.OsAndroid {
		return meta
	}

	meta.Context = normalizeCommandContext(meta.Context, osType)
	if meta.Start != nil {
		meta.Start.Context = normalizeCommandContext(meta.Start.Context, osType)
	}

	return meta
}

func normalizeCommandContext(cmdCtx metadata.Context, osType vmm.OsType) metadata.Context {
	cmdCtx = cmdCtx.WithOsType(string(osType))
	cmdCtx.User = ""
	cmdCtx.WorkDir = nil

	return cmdCtx
}
