#!/usr/bin/env python3
"""TOTP two-factor helper for the end-to-end checklists (standard library only).

  forgejo-2fa.py enroll BASE USER PASSWORD         enrolls TOTP for USER through the web UI,
                                                   prints the TOTP secret
  forgejo-2fa.py code SECRET                       prints the current TOTP code
  forgejo-2fa.py check BASE USER PASSWORD SECRET   GET BASE/api/v1/user with basic auth and the
                                                   current code in X-Forgejo-OTP; prints the HTTP
                                                   status and exits 0 only for 200

BASE is Forgejo's URL without a trailing slash, e.g. http://fj-http.e2e-s3.svc.cluster.local:3000.
"""
import base64
import hashlib
import hmac
import http.cookiejar
import re
import struct
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def totp(secret, at=None):
    key = base64.b32decode(secret.upper() + "=" * (-len(secret) % 8))
    counter = int((time.time() if at is None else at) // 30)
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0F
    return "%06d" % ((struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF) % 1000000)


def session():
    return urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))


def post(opener, url, fields):
    data = urllib.parse.urlencode(fields).encode()
    return opener.open(urllib.request.Request(url, data=data, method="POST"), timeout=30)


def enroll(base, user, password):
    s = session()
    page = post(s, base + "/user/login", {"user_name": user, "password": password}).read().decode()
    if "/user/logout" not in page:
        sys.exit("login as %s failed" % user)
    page = s.open(base + "/user/settings/security/two_factor/enroll", timeout=30).read().decode()
    m = re.search(r"enter the secret: ([A-Z2-7]+)", page)
    if not m:
        sys.exit("no TOTP secret on the enroll page (is two-factor authentication enrolled already?)")
    secret = m.group(1)
    # A code close to the end of its 30 s window may expire on the way.
    if 30 - time.time() % 30 < 3:
        time.sleep(4)
    page = post(s, base + "/user/settings/security/two_factor/enroll", {"passcode": totp(secret)}).read().decode()
    if "two_factor/enroll" in page and "passcode" in page:
        sys.exit("enrolling failed: the passcode was not accepted")
    print(secret)


def check(base, user, password, secret):
    req = urllib.request.Request(base + "/api/v1/user")
    req.add_header("Authorization", "Basic " + base64.b64encode(("%s:%s" % (user, password)).encode()).decode())
    req.add_header("X-Forgejo-OTP", totp(secret))
    try:
        status = urllib.request.urlopen(req, timeout=30).status
    except urllib.error.HTTPError as e:
        status = e.code
    print(status)
    sys.exit(0 if status == 200 else 1)


def main(argv):
    if len(argv) == 5 and argv[1] == "enroll":
        enroll(argv[2].rstrip("/"), argv[3], argv[4])
    elif len(argv) == 3 and argv[1] == "code":
        print(totp(argv[2]))
    elif len(argv) == 6 and argv[1] == "check":
        check(argv[2].rstrip("/"), argv[3], argv[4], argv[5])
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main(sys.argv)
