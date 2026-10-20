FROM python:3.10.12-slim

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONUNBUFFERED=1

WORKDIR /opt/dask-matrix
COPY requirements.txt /opt/dask-matrix/requirements.txt
RUN python -m pip install --no-cache-dir -r requirements.txt \
    && python -m pip check

COPY benchmark.py /opt/dask-matrix/benchmark.py

CMD ["python", "/opt/dask-matrix/benchmark.py"]
