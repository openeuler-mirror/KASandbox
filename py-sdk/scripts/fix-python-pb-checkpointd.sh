#!/bin/bash

rm -rf e2b/checkpointd/__pycache__
rm -rf e2b/checkpointd/checkpoint/__pycache__

sed -i.bak 's/from\ checkpoint\ import/from e2b.checkpointd.checkpoint import/g' e2b/checkpointd/checkpoint/*

rm -f e2b/checkpointd/checkpoint/*.bak