from dotenv import load_dotenv
load_dotenv()
import os
import time
from e2b  import Sandbox
from e2b.api.client.models.numa import NumaBinding

original_time = time.time()
sbx = Sandbox.create("ubuntu-22-04-custom")
print(time.time()-original_time)
print(sbx.is_running())
print(sbx.commands.run("whoami"))  # guest
sbx.kill()
