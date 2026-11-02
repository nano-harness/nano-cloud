# Pull Request

[中文](./PULL_REQUEST_TEMPLATE.zh-CN.md)

## Description

<!-- Briefly describe what this change does and why -->

## Type of Change

- [ ] Bug fix
- [ ] New feature
- [ ] Refactor
- [ ] Docs
- [ ] Other

## Pre-merge Checklist

### Code Quality
- [ ] Tests pass (`go test ./...`)
- [ ] Lint passes (`make lint-check`)
- [ ] Build succeeds (`make build`)
- [ ] Table-driven tests added for branching logic

### Code Review
- [ ] Errors are wrapped with context (`fmt.Errorf("...: %w", err)`)
- [ ] No new security vulnerabilities introduced
- [ ] No sensitive information committed (keys, tokens, etc.)

### Documentation
- [ ] README or related docs updated, both languages in sync (if needed)
- [ ] AGENTS.md updated if documented behavior changed
