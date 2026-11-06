"""Static file server for the installer tests: 127.0.0.1 on a free port; the port goes to argv[2], the request log to stderr."""
import functools
import http.server
import sys

handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=sys.argv[1])
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
with open(sys.argv[2], "w") as port_file:
    port_file.write(str(server.server_address[1]))
server.serve_forever()
