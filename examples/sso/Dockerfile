# TinyStack's own image (the repository's Dockerfile, built by compose.yml as "tinystack-base") plus
# the AWS CLI that the init hook in init/ready.d needs.
FROM base
USER root
RUN apk add --no-cache aws-cli
USER tinystack
