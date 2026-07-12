function check(a,b){if(a!==b)throw a+" !== "+b;} check('John Smith'.replace(/(\w+)\s(\w+)/,'$2 $1'),'Smith John');console.log('PASS');
