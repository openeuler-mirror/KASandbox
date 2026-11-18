#!/usr/bin/env python3
"""生成仅用于本地 TLS 回归的临时 CA；不安装到系统信任库。需要 cryptography。"""
from pathlib import Path
import datetime
from cryptography import x509
from cryptography.x509.oid import NameOID
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
root = Path(__file__).resolve().parent / 'fixtures'
root.mkdir(exist_ok=True)
now = datetime.datetime.now(datetime.timezone.utc)
ca_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, 'Isolated system CA test')])
ca = (x509.CertificateBuilder().subject_name(name).issuer_name(name)
      .public_key(ca_key.public_key()).serial_number(x509.random_serial_number())
      .not_valid_before(now - datetime.timedelta(days=1))
      .not_valid_after(now + datetime.timedelta(days=2))
      .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
      .sign(ca_key, hashes.SHA256()))
(root / 'ca.pem').write_bytes(ca.public_bytes(serialization.Encoding.PEM))
(root / 'empty.pem').write_bytes(b'')
(root / 'empty-dir').mkdir(exist_ok=True)
(root / 'server-key.der').write_bytes(key.private_bytes(
    serialization.Encoding.DER, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
for case in ['valid', 'wrong-host', 'untrusted', 'expired', 'empty']:
    issuer_key = rsa.generate_private_key(public_exponent=65537, key_size=2048) if case == 'untrusted' else ca_key
    end = now - datetime.timedelta(days=1) if case == 'expired' else now + datetime.timedelta(days=1)
    cert = (x509.CertificateBuilder()
            .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, 'localhost')]))
            .issuer_name(name).public_key(key.public_key()).serial_number(x509.random_serial_number())
            .not_valid_before(now - datetime.timedelta(days=3)).not_valid_after(end)
            .add_extension(x509.SubjectAlternativeName([
                x509.DNSName('other.invalid' if case == 'wrong-host' else 'localhost')]), critical=False)
            .sign(issuer_key, hashes.SHA256()))
    (root / (case + '.der')).write_bytes(cert.public_bytes(serialization.Encoding.DER))
print('test fixtures generated')
