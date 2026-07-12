function check(a,b){if(a!==b)throw a+" !== "+b;} var p={};var o=Object.create(p);check(Object.getPrototypeOf(o),p);check(Object.getPrototypeOf({}),Object.prototype);console.log('PASS');
