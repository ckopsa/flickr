#!/usr/bin/env python3
"""Make the Keycloak client the Deck app signs in through.

`flickr-deck` is a PUBLIC client with one way in: the OAuth 2.0 device grant —
a code on the screen, approved on a phone. No secret, no redirect URI, no
browser flow, no password grant. Run again, it brings an existing client back
to that shape rather than failing.

    python3 deck/dev/keycloak-client.py

It asks for a Keycloak admin's name and password (the password is not echoed,
kept or written anywhere) and uses them once for an admin token. flickr itself
then needs the client named in OIDC_DELEGATE_CLIENTS before it takes the token.
"""
import argparse
import getpass
import json
import sys
import urllib.error
import urllib.parse
import urllib.request

CLIENT = {
    'name': 'flickr on a Steam Deck',
    'description': 'The native Deck client: device-code sign-in, no secret.',
    'enabled': True,
    'protocol': 'openid-connect',
    'publicClient': True,
    'standardFlowEnabled': False,
    'implicitFlowEnabled': False,
    'directAccessGrantsEnabled': False,
    'serviceAccountsEnabled': False,
    'attributes': {'oauth2.device.authorization.grant.enabled': 'true'},
}


def call(method, url, token=None, form=None, body=None):
    headers, data = {}, None
    if token:
        headers['Authorization'] = 'Bearer ' + token
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
    elif body is not None:
        data = json.dumps(body).encode()
        headers['Content-Type'] = 'application/json'
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, {'error': raw.decode(errors='replace')[:200]}


def why(body):
    b = body or {}
    return b.get('error_description') or b.get('errorMessage') or b.get('error') or 'no reason given'


def main():
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('--keycloak', default='https://keycloak.kopsa.info')
    ap.add_argument('--realm', default='domestic-realm', help='the realm flickr signs in against')
    ap.add_argument('--admin-realm', default='master', help='the realm the admin account lives in')
    ap.add_argument('--client-id', default='flickr-deck')
    args = ap.parse_args()
    base = args.keycloak.rstrip('/')

    user = input('Keycloak admin username: ').strip()
    password = getpass.getpass('Password (not shown): ')
    form = {'grant_type': 'password', 'client_id': 'admin-cli', 'username': user, 'password': password}
    token_url = '%s/realms/%s/protocol/openid-connect/token' % (base, args.admin_realm)
    status, body = call('POST', token_url, form=form)
    if status == 401:
        # An account with a second factor is refused in the same words as a
        # wrong password, so the code is asked for once before giving up.
        otp = input('One-time code, if the account has one (Enter to skip): ').strip()
        if otp:
            status, body = call('POST', token_url, form=dict(form, totp=otp))
    del password, form
    if status != 200:
        sys.exit('Sign-in refused (%s): %s\nIf the admin account is not in the "%s" realm, pass --admin-realm.'
                 % (status, why(body), args.admin_realm))
    token = body['access_token']

    clients = '%s/admin/realms/%s/clients' % (base, args.realm)
    status, found = call('GET', clients + '?clientId=' + urllib.parse.quote(args.client_id), token=token)
    if status != 200:
        sys.exit('Could not list clients in %s (%s): %s' % (args.realm, status, why(found)))
    want = dict(CLIENT, clientId=args.client_id)
    if found:
        status, body = call('PUT', clients + '/' + found[0]['id'], token=token, body=dict(found[0], **want))
        did = 'updated'
    else:
        status, body = call('POST', clients, token=token, body=want)
        did = 'created'
    if status not in (201, 204):
        sys.exit('Keycloak would not save the client (%s): %s' % (status, why(body)))
    print('Client "%s" %s in realm %s.' % (args.client_id, did, args.realm))

    # The proof is the door itself: ask it for a code, as the Deck will.
    status, body = call('POST', '%s/realms/%s/protocol/openid-connect/auth/device' % (base, args.realm),
                        form={'client_id': args.client_id, 'scope': 'openid profile offline_access'})
    if status == 200:
        print('Device sign-in answers: it offered the code %s at %s' % (body['user_code'], body['verification_uri']))
        print('(That code was only a test; the app will ask for its own.)')
    else:
        print('The client is saved, but the device door refused a test (%s): %s' % (status, why(body)))
        return 1
    print('\nStill to do: add "%s" to OIDC_DELEGATE_CLIENTS in flickr\'s Nomad job and redeploy.' % args.client_id)
    return 0


if __name__ == '__main__':
    sys.exit(main())
