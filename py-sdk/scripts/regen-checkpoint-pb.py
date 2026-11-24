#!/usr/bin/env python3
"""在没有 buf / protoc-gen-connect-python 的机器上重新生成 checkpoint_pb2.py。

正路是 `make generate-checkpointd`（buf + connect-python 插件）。没有那套工具链时
用这个脚本：它**不重新编译 .proto**，而是把现有 pb2 里那段序列化描述符解析出来、
按 .proto 的改动就地修改、再写回去。

这么做是有意的：buf 的 managed 模式会往描述符里塞 java_package / objc_class_prefix
之类一堆选项，用裸 protoc 从 .proto 重新编译会把它们全丢掉，生成物跟正路对不上。
就地修改能保证除了这次真正想改的东西之外，一个字节都不变。

改完会打印新旧描述符的差异，自己核一遍。

用法: python3 scripts/regen-checkpoint-pb.py [--check]
"""
import os
import re
import sys

from google.protobuf import descriptor_pb2

HERE = os.path.dirname(os.path.abspath(__file__))
PB = os.path.join(HERE, "..", "e2b", "checkpointd", "checkpoint", "checkpoint_pb2.py")

# 这一轮要做的改动，和 spec/checkpointd/checkpoint/checkpoint.proto 一一对应
GO_PACKAGE = "github.com/e2b-dev/infra/packages/orchestrator/internal/checkpoint/spec"
NEW_FIELDS = [
    ("CreateCheckpointResponse", "mem_mode", 2),
    ("CheckpointInfo", "mem_mode", 4),
]


def load(path):
    src = open(path, encoding="utf-8").read()
    m = re.search(r"AddSerializedFile\((b'.*?')\)\n", src, re.S)
    if not m:
        sys.exit("在 %s 里找不到 AddSerializedFile" % path)
    return src, m.group(1), eval(m.group(1))  # noqa: S307 生成文件里的字节字面量


def describe(fdp):
    out = []
    for msg in fdp.message_type:
        for f in msg.field:
            out.append("%s.%s = %d (%s)" % (msg.name, f.name, f.number, f.json_name))
    out.append("go_package = %s" % fdp.options.go_package)
    return out


def main():
    src, literal, raw = load(PB)
    fdp = descriptor_pb2.FileDescriptorProto()
    fdp.ParseFromString(raw)
    before = describe(fdp)

    fdp.options.go_package = GO_PACKAGE

    by_name = {m.name: m for m in fdp.message_type}
    for msg_name, field_name, number in NEW_FIELDS:
        msg = by_name[msg_name]
        if any(f.name == field_name for f in msg.field):
            continue
        f = msg.field.add()
        f.name = field_name
        f.number = number
        f.label = descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL
        f.type = descriptor_pb2.FieldDescriptorProto.TYPE_STRING
        # protoc 生成的 json_name 是 lowerCamelCase；connect 的 JSON 编解码用它
        parts = field_name.split("_")
        f.json_name = parts[0] + "".join(p.title() for p in parts[1:])
        # 注意：不设 proto3_optional，也不建合成 oneof —— 用普通 proto3 标量字段，
        # 缺省即空串。加 optional 会引入合成 oneof，对这个字段没有意义。

    after = describe(fdp)
    print("--- 描述符差异 ---")
    for line in sorted(set(after) - set(before)):
        print("  + " + line)
    for line in sorted(set(before) - set(after)):
        print("  - " + line)

    new_raw = fdp.SerializeToString()

    if "--check" in sys.argv:
        print("\n--check：只比对，不写盘")
        return 0

    # protoc 会在生成文件末尾给每个消息/服务标出它在序列化描述符里的字节区间。
    # 这些区间可以直接用"子消息的序列化结果在整份字节里的位置"算出来。
    offsets = {}
    for msg in fdp.message_type:
        blob = msg.SerializeToString()
        i = new_raw.find(blob)
        if i < 0:
            sys.exit("定位不到消息 %s 的字节区间" % msg.name)
        offsets["_" + msg.name.upper()] = (i, i + len(blob))
    for svc in fdp.service:
        blob = svc.SerializeToString()
        i = new_raw.find(blob)
        if i < 0:
            sys.exit("定位不到服务 %s 的字节区间" % svc.name)
        offsets["_" + svc.name.upper()] = (i, i + len(blob))

    out = src.replace(literal, repr(new_raw), 1)
    for key, (a, b) in offsets.items():
        out = re.sub(r"_globals\['%s'\]\._serialized_start=\d+" % re.escape(key),
                     "_globals['%s']._serialized_start=%d" % (key, a), out)
        out = re.sub(r"_globals\['%s'\]\._serialized_end=\d+" % re.escape(key),
                     "_globals['%s']._serialized_end=%d" % (key, b), out)
    out = re.sub(r"(_globals\['DESCRIPTOR'\]\._serialized_options = )b'[^']*'",
                 lambda m: m.group(1) + repr(fdp.options.SerializeToString()), out)

    open(PB, "w", encoding="utf-8").write(out)
    print("\n写回 %s" % os.path.normpath(PB))
    return 0


if __name__ == "__main__":
    sys.exit(main())
