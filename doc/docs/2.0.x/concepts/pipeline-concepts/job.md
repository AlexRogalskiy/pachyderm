# Job

!!! Note "Attention"
         Note that Pachyderm uses the term `job` at two different levels. A global level (check [GlobalID](../../advanced-concepts/globalID) for more details) and jobs that are an execution of a particular pipeline. The following page details the latter.

## Definition

A Pachyderm job is an execution of a pipeline that triggers
when new data is detected in an input repository. Each
job runs your code against the current commit in a `<repo>@<branch>` and
then submits the results to the output repository of the pipeline as a single output commit. A pipeline
triggers a new job every time you submit new changes, a commit, into your
input source.

Each job has an alphanumeric identifier (ID) that you can reference in the `<pipeline>@<jobID>` format.

You can obtain information about all jobs with a given ID by running `list job <jobID>` or restrict to a particular pipeline `list job -p <pipeline>`, or `inspect job <pipeline>@<jobID> --raw`.

## Job Statuses
Each job has the following stages:

| Stage     | Description  |
| --------- | ------------ |
|CREATED| An output commit exists, but the job has not been started by a worker yet.|
|STARTING| The worker has allocated resources for the job (that is, the job counts towards parallelism), but it is still waiting on the inputs to be ready.|
|RUNNING|The worker is processing datums.|
|EGRESS|The worker has completed all the datums but is uploading the output to the egress endpoint.|
|FAILURE|The worker encountered too many errors when processing a datum.|
|KILLED|The job timed out, the output commits were deleted, or a user otherwise called StopJob|
|SUCCESS| None of the bad stuff happened.|

## List Jobs

- The `pachctl list job` command returns list of all global jobs. This command is detailed in [this section of Global ID](../../advanced-concepts/globalID/#list-all-global-commits-and-global-jobs).

- The `pachctl list job <jobID>` commands returns the list of all jobs sharing the same `<jobID>`. This command is detailed in [this section of Global ID](../../advanced-concepts/globalID/#list-all-commits-and-jobs-with-a-global-id). 

- Note that you can also track your jobs downstream as they complete by running `pachctl wait job <jobID>`. 

- The `pachctl list job -p <pipeline>` command returns the jobs in a given pipeline.

    !!! example
        ```shell
        $ pachctl list job -p edges
        ```

        **System Response:**

        ```shell
        ID                               PIPELINE STARTED      DURATION           RESTART PROGRESS  DL       UL       STATE
        fd9454d06d8e4fa38a75c8cd20b39538 edges    20 hours ago 5 seconds          0       2 + 1 / 3 181.1KiB 111.4KiB success
        5a78358d4b53494cbba4550428f2fe98 edges    20 hours ago 2 seconds          0       1 + 0 / 1 57.27KiB 22.22KiB success
        7dcd77a2f7f34ff384a6096d1139e922 edges    20 hours ago Less than a second 0       0 + 0 / 0 0B       0B       success
        ```

    For each job, Pachyderm shows the time the pipeline started with its duration, data downloaded and uploaded, STATE of the pipeline execution, the number of datums in the **PROGRESS** section,  and other information.
    The format of the progress column is `DATUMS PROCESSED + DATUMS SKIPPED / TOTAL DATUMS`.


    For more information, see [Datum Processing States](../../../concepts/pipeline-concepts/datum/datum-processing-states/).

## Inspect Job
The `pachctl inspect job <pipeline>@<jobID>` command enables you to view detailed
information about a specific job in a given pipeline (state, number of datums processed/failed/skipped, data downloaded, uploaded,
process time, image used etc...).

!!! example
    Add a `--raw` flag to output a detailed JSON version of the job.
    ```shell
    $ pachctl inspect job edges@fd9454d06d8e4fa38a75c8cd20b39538 --raw
    ```

    **System Response:**

    ```json
    {
        "job": {
            "pipeline": {
            "name": "edges"
            },
            "id": "fd9454d06d8e4fa38a75c8cd20b39538"
        },
        "pipeline_version": "1",
        "output_commit": {
            "branch": {
            "repo": {
                "name": "edges",
                "type": "user"
            },
            "name": "master"
            },
            "id": "fd9454d06d8e4fa38a75c8cd20b39538"
        },
        "data_processed": "2",
        "data_skipped": "1",
        "data_total": "3",
        "stats": {
            "download_time": "0.113263653s",
            "process_time": "1.020472976s",
            "upload_time": "0.010323995s",
            "download_bytes": "185424",
            "upload_bytes": "114041"
        },
        "state": "JOB_SUCCESS",
        "created": "2021-08-02T20:13:10.461841493Z",
        "started": "2021-08-02T20:13:32.870023561Z",
        "finished": "2021-08-02T20:13:38.691891860Z",
        "details": {
            "transform": {
            "image": "pachyderm/opencv",
            "cmd": [
                "python3",
                "/edges.py"
            ]
            },
            "input": {
            "pfs": {
                "name": "images",
                "repo": "images",
                "repo_type": "user",
                "branch": "master",
                "commit": "fd9454d06d8e4fa38a75c8cd20b39538",
                "glob": "/*"
            }
            },
            "salt": "27bbe39ccae54cc2976e3f960a2e1f94",
            "datum_tries": "3"
        }
    }
    ```



