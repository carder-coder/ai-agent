"""支持 python -m agent.cli 调用。"""

import sys

from .main import main

sys.exit(main())
