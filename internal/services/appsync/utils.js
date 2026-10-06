// The modules APPSYNC_JS code imports: @aws-appsync/utils and
// @aws-appsync/utils/dynamodb. host holds the parts written in Go.
(function (host) {
  const isNull = (v) => v === null || v === undefined;

  const toDynamoDB = (v) => {
    if (isNull(v)) return { NULL: true };
    if (typeof v === "string") return { S: v };
    if (typeof v === "number") return { N: String(v) };
    if (typeof v === "boolean") return { BOOL: v };
    if (Array.isArray(v)) return { L: v.map(toDynamoDB) };
    return { M: toMapValues(v) };
  };

  const toMapValues = (o) => {
    const out = {};
    for (const [k, v] of Object.entries(o || {})) {
      if (v !== undefined) out[k] = toDynamoDB(v);
    }
    return out;
  };

  // The operators of enhanced subscription filters.
  const filterOperators = [
    "eq",
    "ne",
    "le",
    "lt",
    "ge",
    "gt",
    "contains",
    "notContains",
    "beginsWith",
    "in",
    "notIn",
    "between",
    "containsAny",
  ];

  // fieldFilters are the filters { operator: value } sets on a field, whose
  // nested objects name its fields.
  const fieldFilters = (path, ops) =>
    Object.entries(ops).flatMap(([op, value]) =>
      filterOperators.includes(op) ? [{ fieldName: path, operator: op, value }] : fieldFilters(`${path}.${op}`, value),
    );

  // filterGroups converts a filter object into groups of filters, any of
  // which may pass, each of whose filters must.
  const filterGroups = (o, ignored) => {
    const and = (a, b) => a.flatMap((x) => b.map((y) => x.concat(y)));
    let groups = [[]];
    for (const [key, v] of Object.entries(o || {})) {
      if (ignored.includes(key)) continue;
      if (key === "or") groups = and(groups, v.flatMap((e) => filterGroups(e, ignored)));
      else if (key === "and") for (const e of v) groups = and(groups, filterGroups(e, ignored));
      else groups = and(groups, [fieldFilters(key, v)]);
    }
    return groups;
  };

  const util = {
    error: host.error,
    appendError: host.appendError,
    unauthorized: host.unauthorized,
    autoId: host.autoId,
    autoUlid: host.autoUlid,
    base64Encode: host.base64Encode,
    base64Decode: host.base64Decode,
    urlEncode: encodeURIComponent,
    urlDecode: decodeURIComponent,
    escapeJavaScript: (s) => JSON.stringify(String(s)).slice(1, -1),
    isNull,
    isNullOrEmpty: (v) => isNull(v) || v === "" || (Array.isArray(v) && v.length === 0),
    isNullOrBlank: (v) => isNull(v) || String(v).trim() === "",
    defaultIfNull: (v, d) => (isNull(v) ? d : v),
    defaultIfNullOrEmpty: (v, d) => (isNull(v) || v === "" ? d : v),
    defaultIfNullOrBlank: (v, d) => (isNull(v) || String(v).trim() === "" ? d : v),
    matches: (pattern, s) => new RegExp(pattern).test(s),
    time: {
      nowISO8601: () => new Date().toISOString(),
      nowEpochSeconds: () => Math.floor(Date.now() / 1000),
      nowEpochMilliSeconds: () => Date.now(),
      parseISO8601ToEpochMilliSeconds: (s) => Date.parse(s),
      epochMilliSecondsToSeconds: (ms) => Math.floor(ms / 1000),
      epochMilliSecondsToISO8601: (ms) => new Date(ms).toISOString(),
    },
    transform: {
      toSubscriptionFilter: (o, ignoredFields = [], rules = undefined) => {
        if (rules !== undefined) throw new Error("TinyStack does not support toSubscriptionFilter rules.");
        return { filterGroup: filterGroups(o, ignoredFields).map((filters) => ({ filters })) };
      },
    },
    dynamodb: {
      toDynamoDB,
      toMapValues,
      toString: (s) => ({ S: s }),
      toNumber: (n) => ({ N: String(n) }),
      toBoolean: (b) => ({ BOOL: b }),
      toNull: () => ({ NULL: true }),
      toList: (l) => ({ L: l.map(toDynamoDB) }),
      toMap: (m) => ({ M: toMapValues(m) }),
      toStringSet: (l) => ({ SS: l }),
      toNumberSet: (l) => ({ NS: l.map(String) }),
    },
  };

  // expression builds a DynamoDB expression from the filter and condition
  // objects the dynamodb module takes: { field: { operator: value }, and,
  // or, not }.
  const expression = (input) => {
    if (isNull(input)) return undefined;
    const names = {};
    const values = {};
    let n = 0;
    const name = (field) => {
      const key = "#" + field.replace(/[^A-Za-z0-9_]/g, "_");
      names[key] = field;
      return key;
    };
    const value = (v) => {
      const key = ":v" + n++;
      values[key] = toDynamoDB(v);
      return key;
    };
    const comparisons = { eq: "=", ne: "<>", lt: "<", le: "<=", gt: ">", ge: ">=" };
    const build = (o) => {
      const parts = [];
      for (const [field, ops] of Object.entries(o)) {
        if (field === "and" || field === "or") {
          parts.push("(" + ops.map(build).join(field === "and" ? " AND " : " OR ") + ")");
          continue;
        }
        if (field === "not") {
          parts.push("(NOT " + build(ops) + ")");
          continue;
        }
        for (const [op, v] of Object.entries(ops)) {
          const f = name(field);
          if (comparisons[op]) parts.push(`${f} ${comparisons[op]} ${value(v)}`);
          else if (op === "between") parts.push(`${f} BETWEEN ${value(v[0])} AND ${value(v[1])}`);
          else if (op === "beginsWith") parts.push(`begins_with(${f}, ${value(v)})`);
          else if (op === "contains") parts.push(`contains(${f}, ${value(v)})`);
          else if (op === "notContains") parts.push(`NOT contains(${f}, ${value(v)})`);
          else if (op === "attributeExists") parts.push(v ? `attribute_exists(${f})` : `attribute_not_exists(${f})`);
          else if (op === "attributeType") parts.push(`attribute_type(${f}, ${value(v)})`);
          else throw new Error(`TinyStack does not support the ${op} operator.`);
        }
      }
      return parts.join(" AND ");
    };
    return { expression: build(input), expressionNames: names, expressionValues: values };
  };

  const operation = (op) => (value) => ({ __op: op, value });
  const operations = {
    add: operation("add"),
    remove: operation("remove"),
    replace: operation("replace"),
    increment: operation("increment"),
    decrement: operation("decrement"),
    append: operation("append"),
    prepend: operation("prepend"),
    updateListItem: (value, index) => ({ __op: "updateListItem", value, index }),
  };

  // updateExpression builds an UpdateItem expression from { field: value or
  // operation }.
  const updateExpression = (fields) => {
    const names = {};
    const values = {};
    const set = [];
    const remove = [];
    const add = [];
    let n = 0;
    const value = (v) => {
      const key = ":u" + n++;
      values[key] = toDynamoDB(v);
      return key;
    };
    for (const [field, v] of Object.entries(fields)) {
      const f = "#" + field.replace(/[^A-Za-z0-9_]/g, "_");
      names[f] = field;
      const op = v && v.__op;
      if (!op || op === "replace") set.push(`${f} = ${value(op ? v.value : v)}`);
      else if (op === "remove") remove.push(f);
      else if (op === "increment") set.push(`${f} = ${f} + ${value(v.value)}`);
      else if (op === "decrement") set.push(`${f} = ${f} - ${value(v.value)}`);
      else if (op === "append") set.push(`${f} = list_append(${f}, ${value(v.value)})`);
      else if (op === "prepend") set.push(`${f} = list_append(${value(v.value)}, ${f})`);
      else if (op === "updateListItem") set.push(`${f}[${Number(v.index)}] = ${value(v.value)}`);
      else if (op === "add") add.push(`${f} ${value(v.value)}`);
    }
    const clauses = [];
    if (set.length) clauses.push("SET " + set.join(", "));
    if (remove.length) clauses.push("REMOVE " + remove.join(", "));
    if (add.length) clauses.push("ADD " + add.join(", "));
    return { expression: clauses.join(" "), expressionNames: names, expressionValues: values };
  };

  const dynamodb = {
    operations,
    get: ({ key, consistentRead }) => ({ operation: "GetItem", key: toMapValues(key), consistentRead }),
    put: ({ key, item, condition }) => ({
      operation: "PutItem",
      key: toMapValues(key),
      attributeValues: toMapValues(item),
      condition: expression(condition),
    }),
    remove: ({ key, condition }) => ({ operation: "DeleteItem", key: toMapValues(key), condition: expression(condition) }),
    update: ({ key, update, condition }) => ({
      operation: "UpdateItem",
      key: toMapValues(key),
      update: updateExpression(update),
      condition: expression(condition),
    }),
    query: ({ query, filter, index, limit, nextToken, scanIndexForward, consistentRead }) => ({
      operation: "Query",
      query: expression(query),
      filter: expression(filter),
      index,
      limit,
      nextToken,
      scanIndexForward,
      consistentRead,
    }),
    scan: ({ filter, index, limit, nextToken, consistentRead }) => ({
      operation: "Scan",
      filter: expression(filter),
      index,
      limit,
      nextToken,
      consistentRead,
    }),
  };

  return {
    "@aws-appsync/utils": {
      util,
      runtime: { earlyReturn: host.earlyReturn },
      extensions: {
        setSubscriptionFilter: host.setSubscriptionFilter,
        setSubscriptionInvalidationFilter: host.invalidation,
        invalidateSubscriptions: host.invalidation,
      },
    },
    "@aws-appsync/utils/dynamodb": dynamodb,
  };
})
