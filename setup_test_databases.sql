-- This is an example script for creating test databases.

-- The rest of the script relies on psql variables primary_test_database_name and copied_test_database_count. In this
-- example, those values are read from the environment. You can use different environment variables, hard-code the
-- values, \prompt, etc. to set these.
\set primary_test_database_name `echo $TEST_DATABASE`
\set copied_test_database_count `echo $TEST_DATABASE_COUNT`

-- Drop and recreate the primary test database.
drop database if exists :primary_test_database_name;
create database :primary_test_database_name;

-- Connect to the primary test database.
\c :primary_test_database_name

-- Do your test database setup here such as running migrations and load test data.
--
-- For example, if you wanted to shell out to tern to run your migrations.
-- \! tern migrate

-- The following code creates the table used by testdb to manage access to the test databases, creates a row for each
-- test database, then drops and recreates each test database using the original test database as a template. This code
-- should not be changed.
create schema testdb;
create table testdb.databases (name text primary key, acquirer_pid int);

insert into testdb.databases (name)
select :'primary_test_database_name' || '_' || n
from generate_series(1, :copied_test_database_count) n;

select format('drop database if exists %I with (force)', name),
	format('create database %I template = %I', name, :'primary_test_database_name')
from testdb.databases
\gexec
