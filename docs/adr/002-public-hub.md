# 002: Explicit public proxy origin and persistent clients

A public hub opts in with `serve --public-url https://host`. This extends D1 and D13:
the HTTP listener may remain loopback behind a TLS reverse proxy. The proxy must
preserve Host, remove incoming forwarding headers, and set X-Forwarded-Proto itself.
Only a loopback peer with that exact public Host and `X-Forwarded-Proto: https` may
create or use a UI session. Without the flag, the loopback peer and loopback Host
requirements remain. Public cookies are Secure as well as HttpOnly and SameSite=Strict.
Bearer authentication still gates creation of the short-lived, single-use login link.
This trusts the operator's proxy configuration; it cannot verify the outer TLS hop.

D14 also gains a user-level client.json beside the default token location, allowing
GUI-launched clients to discover the hub without inherited environment variables.
Environment values override this file. Setup may restrict integration to selected
clients and does not install a service for a remote hub.
