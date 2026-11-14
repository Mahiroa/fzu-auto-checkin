"""让 pytest 在任意工作目录下都能 import src.*（项目根加入 sys.path）。"""
import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if ROOT not in sys.path:
    sys.path.insert(0, ROOT)
