# 提示词回归用例

提示词改动改的是模型行为，不是代码分支，单元测试覆盖不到。Go 测试只保证结构（某句话在该出现时出现、位置对、未配置时的退化文案），行为得靠这里的用例，用真实模型跑、人工读 transcript 判分。

## 怎么用

1. 每个用例一个文件，编号对应 `spec/prompt-tuning.md` 第 6 节的案例台账。
2. 判定标准必须**可观察**：「回答得更好」不可判定，「首次 websearch 的 query 含 qwen3.8」可判定。
3. 每次提示词改动后，用 2～3 个线上常用模型各跑一遍，把结果追加到用例文件末尾的「历次结果」表里，并同步回台账的「复发情况」。
4. **不要用假模型跑**：假模型的行为是我们写死的，验证不了任何东西。

## 怎么读 transcript

会话的原始消息在 `<数据目录>/sessions/<会话 id>/transcript/chat.jsonl`，一行一条消息。看工具调用的参数和模型的 thinking：

```bash
python3 -c '
import json
for line in open("chat.jsonl"):
    d = json.loads(line)
    for c in d.get("message", {}).get("content", []):
        if c.get("type") == "toolCall":
            print(c["name"], json.dumps(c["arguments"], ensure_ascii=False))
        if c.get("type") == "thinking":
            print("[think]", c["thinking"][:200])
'
```

生产库只读，不要在上面改配置。
