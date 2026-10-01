// Schema/semantic fixture checks for the proposed contract, not backend execution.
// Dependencies are read from a caller-supplied existing package.json; no install.
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import assert from 'node:assert/strict';

const fixtureDir = dirname(fileURLToPath(import.meta.url));
const contractDir = resolve(fixtureDir, '../..');
const packagePath = process.env.DISTRIBUTION_VALIDATION_PACKAGE_JSON;
if (!packagePath) throw new Error('Set DISTRIBUTION_VALIDATION_PACKAGE_JSON to package.json containing @redocly/ajv 8.x.');
const require = createRequire(resolve(packagePath));
const Ajv2020 = require('@redocly/ajv/dist/2020').default;
const ajv = new Ajv2020({ strict: false, allErrors: true });
ajv.addFormat('uuid', /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
ajv.addFormat('date-time', { type: 'string', validate: s => /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/.test(s) && !Number.isNaN(Date.parse(s)) });
ajv.addFormat('uri', { type: 'string', validate: s => { try { return Boolean(new URL(s).protocol); } catch { return false; } } });
const docs = {};
const filenames = { teamos: 'teamos-distribution.openapi.yaml', core: 'core-distribution.openapi.yaml' };
for (const [key, name] of Object.entries(filenames)) {
  const parsed = spawnSync(process.env.PYTHON || 'python3', ['-c', 'import yaml,json,sys; print(json.dumps(yaml.safe_load(open(sys.argv[1]))))', resolve(contractDir, name)], { encoding: 'utf8' });
  if (parsed.status !== 0) throw new Error(parsed.stderr);
  docs[key] = JSON.parse(parsed.stdout);
  ajv.addSchema(docs[key], `https://distribution.example.invalid/${name}`);
}
const compiled = {};
for (const [key, doc] of Object.entries(docs)) {
  const ids = new Set();
  assert.equal(doc['x-contract-status'], 'proposed');
  for (const schema of Object.keys(doc.components.schemas)) {
    compiled[`${key}:${schema}`] = ajv.compile({ $ref: `https://distribution.example.invalid/${filenames[key]}#/components/schemas/${schema}` });
  }
  for (const [path, resource] of Object.entries(doc.paths)) {
    for (const [method, operation] of Object.entries(resource)) {
      if (!['get', 'post', 'put', 'patch', 'delete'].includes(method)) continue;
      assert(!ids.has(operation.operationId), `Duplicate operationId ${operation.operationId}`);
      ids.add(operation.operationId);
      for (const id of [...path.matchAll(/\{([^}]+)\}/g)].map(match => match[1])) {
        assert([...resource.parameters ?? [], ...operation.parameters ?? []].some(parameter => parameter.in === 'path' && parameter.name === id && parameter.required), `Missing path parameter ${id}`);
      }
      if (['post', 'put', 'patch', 'delete'].includes(method) && !path.endsWith('/validate')) {
        assert(operation.parameters.some(p => p.$ref === '#/components/parameters/IdempotencyKey'), `Missing idempotency ${path}`);
        assert(operation.responses['409'], `Missing idempotency/CAS conflict ${path}`);
      }
      if (path.includes('/internal/') && ['events', 'results', 'assignments', 'cancel', 'reconcile'].some(suffix => path.endsWith(`/${suffix}`))) assert(operation.responses['202']);
    }
  }
}

function semantic(schema, value) {
  function walk(node, key = '') {
    if (!node || typeof node !== 'object') {
      if (typeof node === 'string' && /^(accountId|leadId|pipelineId|statusId|crmUserId|verifiedCrmUserId|responsibleUserId|targetResponsibleUserId)$/.test(key) && BigInt(node) > 9223372036854775807n) throw new Error('CRM ID above bigint');
      return;
    }
    for (const [k, v] of Object.entries(node)) walk(v, k);
  }
  walk(value);
  if (schema === 'UpdateRuleInput' || schema === 'CreateRuleInput') {
    if (value.disabledMemberIds.some(id => !value.memberIds.includes(id))) throw new Error('Disabled member outside group');
  }
  if (schema === 'AssignmentEnvelope' && value.command.decisionKind === 'keep') {
    if (value.command.targetResponsibleUserId !== value.command.expectedSnapshot.responsibleUserId) throw new Error('Keep changes target');
  }
  const result = schema === 'Operation' ? value : schema === 'ResultEnvelope' ? value.result : null;
  if (result && ['succeeded', 'no_change'].includes(result.state)) {
    if (result.confirmedSnapshot.responsibleUserId !== result.targetResponsibleUserId) throw new Error('Result target mismatch');
  }
  if (schema === 'ResultEnvelope') {
    assert.deepEqual(value.scope, value.result.scope, 'Result scope mismatch');
    assert.equal(value.eventId, value.result.eventId);
    assert.equal(value.correlationId, value.result.correlationId);
  }
}

const cases = JSON.parse(readFileSync(resolve(fixtureDir, 'schema-cases.json'), 'utf8'));
for (const fixture of cases) {
  const validate = compiled[`${fixture.contract}:${fixture.schema}`];
  assert(validate, fixture.name);
  let valid = validate(fixture.value);
  let detail = JSON.stringify(validate.errors);
  if (valid) {
    try { semantic(fixture.schema, fixture.value); } catch (error) { valid = false; detail = error.message; }
  }
  assert.equal(valid, fixture.valid, `${fixture.name}: ${detail}`);
}

const proto = readFileSync(resolve(contractDir, 'distribution.proto'), 'utf8');
for (const [name, prefix] of [['QueueState', 'QUEUE_STATE'], ['OperationState', 'OPERATION_STATE'], ['ExternalEffectState', 'EXTERNAL_EFFECT_STATE'], ['OperationOutcome', 'OPERATION_OUTCOME']]) {
  const section = proto.match(new RegExp(`enum ${name} \\{([\\s\\S]*?)\\}`))[1];
  const wire = [...section.matchAll(new RegExp(`${prefix}_([A-Z_]+) = \\d+;`, 'g'))].map(match => match[1].toLowerCase()).filter(value => value !== 'unspecified');
  const schema = docs.teamos.components.schemas[name] ?? docs.core.components.schemas[name];
  assert.deepEqual(wire, schema.enum, `${name} proto/OpenAPI drift`);
}
console.log(`${cases.length} schema/semantic fixtures passed; ${Object.keys(compiled).length} schemas compiled; route invariants and four proto enum mappings passed.`);
console.log('This validates the specification only; idempotency persistence, RBAC, CRM races and service handlers require runtime integration tests.');
