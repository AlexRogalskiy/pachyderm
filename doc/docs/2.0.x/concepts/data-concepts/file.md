# File

A file is a Unix filesystem object, which is a directory or
file, that stores data. Unlike source code
version-control systems that are most suitable for storing plain text
files, you can store any type of file in Pachyderm, including
binary files. Often, data scientists operate with
comma-separated values (CSV), JavaScript Object Notation (JSON),
images, and other plain text and binary file
formats. Pachyderm supports all file sizes and formats and applies
storage optimization techniques, such as deduplication, in the
background.

To upload your files to a Pachyderm repository, run the
`pachctl put file` command. By using the `pachctl put file`
command, you can put both files and directories into a Pachyderm repository.

!!! Warning
     It is important to note that **directories are implied from the paths of the files**. Directories are not stored and will not exist **unless they contain files**. 
     
## File Processing Strategies

Pachyderm provides the following file processing strategies:

### **Overwriting Files**
By default, when you put a file into a Pachyderm repository and a
file by the same name already exists in the repo, Pachyderm overwrites
the existing file with the new data.
For example, you have an `A.csv` file in a repository. If you upload the
same file to that repository, Pachyderm *overwrites* the existing
file with the data, which results in the `A.csv` file having only data
from the most recent upload.

!!! example

    1. View the list of files:

         ```shell
         pachctl list file images@master
         ```

         **System Response:**

         ```shell
         NAME   TYPE SIZE
         /A.csv file 258B
         ```

    1. Add the `A.csv` file once again:

         ```shell
         pachctl put file images@master -f A.csv
         ```

    1. Verify that the file size has not changed:

         ```shell
         pachctl list file images@master
         ```

         **System Response:**

         ```shell
         NAME   TYPE SIZE
         /A.csv file 258B
         ```

### **Appending to files**
When you enable the append mode by using the `--append`
flag or `-a`, the new files are appended to existing ones instead of overwriting them.
For example, you have an `A.csv` file in the `images` repository.
If you upload the same file to that repository with the
`--append` flag, Pachyderm *appends* to the file.

!!! example

    1. View the list of files:

         ```shell
         pachctl list file images@master
         ```

         **System Response:**

         ```shell
         NAME   TYPE SIZE
         /A.csv file 258B
         ```

    1. Add the `A.csv` file once again:

         ```shell
         pachctl put file -a images@master -f A.csv
         ```

    1. Verify that the file size has doubled:

         ```shell
         pachctl list file images@master
         ```

         **System Response:**

         ```shell
         NAME   TYPE SIZE
         /A.csv file 516B
         ```
