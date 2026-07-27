#!/system/bin/sh
set -e

# Android golden images already contain their account and envd setup. Keep
# finalize marker-only and place the marker on writable /data.
cat <<EOF > /data/.e2b
ENV_ID={{ .TemplateID }}
TEMPLATE_ID={{ .TemplateID }}
BUILD_ID={{ .BuildID }}
EOF
