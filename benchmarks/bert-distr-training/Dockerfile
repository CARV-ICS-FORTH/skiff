FROM python:3.11.14-slim-bookworm
RUN python -m pip install --no-cache-dir torch==2.7.1 \
    --index-url https://download.pytorch.org/whl/cpu
COPY requirements.txt /tmp/requirements.txt
RUN python -m pip install --no-cache-dir -r /tmp/requirements.txt \
    && python -m pip check \
    && rm /tmp/requirements.txt
ENV CUDA_VISIBLE_DEVICES="" PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    TOKENIZERS_PARALLELISM=false
WORKDIR /benchmark
