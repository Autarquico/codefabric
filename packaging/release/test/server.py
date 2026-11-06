"""Static file server for the installer tests: 127.0.0.1 on a free port; the port goes to argv[2], the request log to stderr.

A request for <path>/latest is answered with a 302 to the Location held in the file <path>/latest.redirect when that
file exists, the way github.com/<repo>/releases/latest redirects to .../releases/tag/vX.Y.Z.
"""
import functools
import http.server
import os
import sys


class Handler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        redirect = os.path.join(self.directory, self.path.lstrip("/") + ".redirect")
        if self.path.endswith("/latest") and os.path.isfile(redirect):
            with open(redirect) as f:
                location = f.read().strip()
            self.send_response(302)
            self.send_header("Location", location)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        super().do_GET()


handler = functools.partial(Handler, directory=sys.argv[1])
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
with open(sys.argv[2], "w") as port_file:
    port_file.write(str(server.server_address[1]))
server.serve_forever()
