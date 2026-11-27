#!/usr/bin/env python3
"""Security group rule descriptions may only use EC2's character set; cfn-lint doesn't
check it, and a bad one fails the deploy (M4 lab: an apostrophe rolled back the app stack).
Checks every `Description:` inside a SecurityGroupIngress/Egress resource, and inside
SecurityGroupIngress/Egress lists of a SecurityGroup. Usage: check-sg-descriptions.py FILE..."""
import re
import sys

ALLOWED = set("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789. _-:/()#,@[]+=&;{}!$*")
bad = 0
for path in sys.argv[1:]:
    in_rule = False
    for n, line in enumerate(open(path), 1):
        if re.match(r"^  \S", line):  # a new top-level resource or section
            in_rule = False
        if re.search(r"Type: AWS::EC2::SecurityGroup(Ingress|Egress)\b|^\s+SecurityGroup(Ingress|Egress):", line):
            in_rule = True
        m = re.match(r"^\s+(?:- )?Description:\s*(.+)$", line)
        if in_rule and m:
            text = m.group(1).strip().strip("'\"")
            wrong = sorted(set(text) - ALLOWED)
            if wrong or len(text) >= 256:
                print(f"{path}:{n}: rule description has {wrong or 'over 255 characters'}: {text}")
                bad += 1
sys.exit(1 if bad else 0)
