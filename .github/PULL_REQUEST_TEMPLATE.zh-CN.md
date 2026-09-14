# Pull Request

[English](./PULL_REQUEST_TEMPLATE.md)

## 变更说明

<!-- 简要描述这个变更做了什么、为什么做 -->

## 变更类型

- [ ] Bug 修复
- [ ] 新功能
- [ ] 重构
- [ ] 文档
- [ ] 其他

## 合并前检查清单

### 代码质量
- [ ] 测试通过（`go test ./...`）
- [ ] Lint 通过（`make lint-check`）
- [ ] 构建成功（`make build`）
- [ ] 分支逻辑已补充表驱动测试

### 代码审查
- [ ] 错误处理带上下文包装（`fmt.Errorf("...: %w", err)`）
- [ ] 未引入新的安全漏洞
- [ ] 未提交敏感信息（密钥、token 等）

### 文档
- [ ] README 或相关文档已更新，中英双语同步（如需要）
- [ ] 若变更了 AGENTS.md 已文档化的行为，已同步更新 AGENTS.md
