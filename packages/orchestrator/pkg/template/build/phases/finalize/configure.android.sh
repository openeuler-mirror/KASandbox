#!/system/bin/sh
set -e

# Use the image's eth1 route (169.254.0.21 via 169.254.0.22, table 200)
ip -4 rule show | grep -Eq '^31999:[[:space:]]+from all lookup 200[[:space:]]*$' || \
    ip -4 rule add pref 31999 from all lookup 200

# Android golden images already contain their account and envd setup.
# Place the marker on writable /data.
cat <<EOF > /data/.e2b
ENV_ID={{ .TemplateID }}
TEMPLATE_ID={{ .TemplateID }}
BUILD_ID={{ .BuildID }}
EOF
